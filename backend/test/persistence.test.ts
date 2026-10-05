import { createHash } from "node:crypto";
import {spawn} from "node:child_process";
import { appendFile, mkdir, readFile, readdir, rm, writeFile } from "node:fs/promises";
import { mkdtemp } from "node:fs/promises";
import {hostname, tmpdir} from "node:os";
import { dirname, join } from "node:path";
import test from "node:test";
import assert from "node:assert/strict";

import {
  DEFAULT_RUN_ERROR_CACHE_MAX_ENTRIES,
  MAX_CURSOR_CACHE_CHECKPOINT_BYTES,
  MAX_RUN_ERROR_CACHE_MAX_ENTRIES,
  PersistenceError,
  SESSION_INPUT_BATCH_ID_FIELD,
  SESSION_INPUT_BATCH_INDEX_FIELD,
  SessionResponseStore,
  readTranscriptRecords,
  type ResponseCompletedEvent,
} from "../src/persistence.js";
import {persistResponseStream} from "../src/responses.js";
import {
  RESPONSE_PARTIAL_CLEANUP_LOCK_FILE,
  RESPONSE_PARTIAL_COMPACT_FILE,
  RESPONSE_PARTIAL_FILE,
  RESPONSE_PARTIAL_OWNER_FILE,
} from "../src/response-partial-files.js";
import { ConfigError, SessionConfigLoader, resolveApiKey, sha256Json } from "../src/config.js";
import { AgentSession } from "../src/session.js";
import {TaskStore} from "../src/tools/native/task-store.js";
import type OpenAI from "openai";

async function makeSession(): Promise<string> {
  return mkdtemp(join(tmpdir(), "agent-harness-persistence-"));
}

async function removeSession(path: string): Promise<void> {
  await rm(path, { recursive: true, force: true });
}

function completedEvent(
  output: Array<Record<string, unknown>>,
  responseId = "resp_test",
): ResponseCompletedEvent {
  return {
    type: "response.completed",
    response: {
      id: responseId,
      object: "response",
      status: "completed",
      model: "gpt-test",
      usage: { input_tokens: 10, output_tokens: 4 },
      output,
    },
  };
}

function usageEvent(
  output: Array<Record<string, unknown>>,
  responseId: string,
  usage: Record<string, unknown>,
  sessionTurn: number,
  sessionStep: number,
): ResponseCompletedEvent {
  const event = completedEvent(output, responseId);
  return {
    ...event,
    session_turn: sessionTurn,
    session_step: sessionStep,
    response: {...event.response, usage},
  };
}

test("resets a profile transcript and derived artifacts but retains run journals", async () => {
  const sessionDir = await makeSession();
  const messagesBackupPath = join(sessionDir, "profile", "messages.backup.jsonl");
  const compactionCachePath = join(sessionDir, "profile", "staging", "compaction-cache.json");
  const compactionCacheBackupPath = `${compactionCachePath}.backup`;
  const historyCachePath = join(sessionDir, "profile", "staging", "history-cache.json");
  const usageCachePath = join(sessionDir, "profile", "staging", "usage-cache.json");
  const cursorCachePath = join(sessionDir, "profile", "staging", "cursor-cache.json");
  const stateContextPath = join(sessionDir, "profile", "staging", "statecontext.json");
  const deletedHistoryArchivePath = join(
    sessionDir,
    "profile",
    "messages.jsonl.deleted-history",
    "messages-20260926T040306587Z-12345678.jsonl",
  );
  const runJournalPath = join(sessionDir, "profile", "staging", "run-1", "manifest.json");
  try {
    const store = await SessionResponseStore.open(sessionDir, {
      messagesPath: "profile/messages.jsonl",
      stagingDir: "profile/staging",
      messagesBackupPath: "profile/messages.backup.jsonl",
      compactionCachePath: "profile/staging/compaction-cache.json",
      historyCachePath: "profile/staging/history-cache.json",
      usageCachePath: "profile/staging/usage-cache.json",
      cursorCachePath: "profile/staging/cursor-cache.json",
      stateContextPath: "profile/staging/statecontext.json",
    });
    await store.appendInputItem({
      type: "message",
      role: "user",
      content: [{type: "input_text", text: "before reset"}],
    });
    assert.equal((await store.readUsageSummary()).turn_count, 1);

    for (const path of [
      messagesBackupPath,
      compactionCachePath,
      compactionCacheBackupPath,
      historyCachePath,
      usageCachePath,
      cursorCachePath,
      stateContextPath,
    ]) {
      await mkdir(dirname(path), {recursive: true});
      await writeFile(path, "stale artifact");
    }
    await mkdir(dirname(runJournalPath), {recursive: true});
    await writeFile(runJournalPath, "preserve recovery record");
    await mkdir(dirname(deletedHistoryArchivePath), {recursive: true});
    await writeFile(deletedHistoryArchivePath, "recoverable front-compaction tail");

    await store.reset();

    assert.equal(await readFile(store.messagesPath, "utf8"), "");
    assert.deepEqual(await store.readUsageSummary(), {
      completed_responses: 0,
      responses_with_usage: 0,
      turn_count: 0,
      history_partial: false,
    });
    for (const path of [
      messagesBackupPath,
      compactionCachePath,
      compactionCacheBackupPath,
      historyCachePath,
      usageCachePath,
      cursorCachePath,
      stateContextPath,
    ]) {
      await assert.rejects(readFile(path, "utf8"), {code: "ENOENT"});
    }
    assert.equal(await readFile(runJournalPath, "utf8"), "preserve recovery record");
    await assert.rejects(readFile(deletedHistoryArchivePath, "utf8"), {code: "ENOENT"});
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("front compaction truncates from the start of the active user batch and rejects backup ids", async () => {
  const sessionDir = await makeSession();
  const messagesBackupPath = join(sessionDir, "messages.backup.jsonl");
  const user = (
    text: string,
    batch?: {id: string; index: number},
  ): Record<string, unknown> => ({
    type: "message",
    role: "user",
    content: [{type: "input_text", text}],
    ...(batch === undefined ? {} : {
      [SESSION_INPUT_BATCH_ID_FIELD]: batch.id,
      [SESSION_INPUT_BATCH_INDEX_FIELD]: batch.index,
    }),
  });
  const response = (id: string, text: string): Record<string, unknown> => ({
    type: "response.completed",
    response: {
      id,
      object: "response",
      status: "completed",
      model: "gpt-test",
      usage: {input_tokens: 10, output_tokens: 2},
      output: [{type: "message", role: "assistant", content: [{type: "output_text", text}]}],
    },
  });
  const records = [
    user("run one"),
    response("resp_one", "answer one"),
    user("run two part one", {id: "batch-two", index: 0}),
    user("run two part two", {id: "batch-two", index: 1}),
    response("resp_two", "answer two"),
    user("run three"),
    response("resp_three", "answer three"),
  ];
  const transcript = `${records.map((record) => JSON.stringify(record)).join("\n")}\n`;
  const runTwoSecondInputOffset = Buffer.byteLength(
    `${records.slice(0, 3).map((record) => JSON.stringify(record)).join("\n")}\n`,
    "utf8",
  );
  const preservedPrefix = `${records.slice(0, 2).map((record) => JSON.stringify(record)).join("\n")}\n`;
  let store: SessionResponseStore | undefined;
  try {
    store = await SessionResponseStore.open(sessionDir, {messagesBackupPath});
    await writeFile(store.messagesPath, transcript);
    await writeFile(messagesBackupPath, "backup history remains read-only\n");

    await assert.rejects(
      store.frontCompactFromMessage(`session-user-b${runTwoSecondInputOffset}`, "permanent"),
      PersistenceError,
    );
    assert.equal(await readFile(store.messagesPath, "utf8"), transcript);

    const result = await store.frontCompactFromMessage(
      `session-user-o${runTwoSecondInputOffset}`,
      "permanent",
    );
    assert.deepEqual(result, {delete_mode: "permanent"});
    assert.equal(await readFile(store.messagesPath, "utf8"), preservedPrefix);
    assert.equal(await readFile(messagesBackupPath, "utf8"), "backup history remains read-only\n");
    assert.equal((await store.readUsageSummary()).turn_count, 1);
  } finally {
    await store?.close();
    await removeSession(sessionDir);
  }
});

test("recoverable front compaction archives the removed active JSONL tail with a date name", async () => {
  const sessionDir = await makeSession();
  let store: SessionResponseStore | undefined;
  try {
    store = await SessionResponseStore.open(sessionDir);
    const records = [
      {
        type: "message",
        role: "user",
        content: [{type: "input_text", text: "keep this run"}],
      },
      {
        type: "response.completed",
        response: {
          id: "resp_keep",
          object: "response",
          status: "completed",
          model: "gpt-test",
          usage: {input_tokens: 10, output_tokens: 2},
          output: [{type: "message", role: "assistant", content: [{type: "output_text", text: "kept"}]}],
        },
      },
      {
        type: "message",
        role: "user",
        content: [{type: "input_text", text: "archive this run"}],
      },
      {
        type: "response.completed",
        response: {
          id: "resp_remove",
          object: "response",
          status: "completed",
          model: "gpt-test",
          usage: {input_tokens: 15, output_tokens: 3},
          output: [{type: "message", role: "assistant", content: [{type: "output_text", text: "removed"}]}],
        },
      },
    ];
    const transcript = `${records.map((record) => JSON.stringify(record)).join("\n")}\n`;
    const firstRemovedOffset = Buffer.byteLength(
      `${records.slice(0, 2).map((record) => JSON.stringify(record)).join("\n")}\n`,
      "utf8",
    );
    const preservedPrefix = `${records.slice(0, 2).map((record) => JSON.stringify(record)).join("\n")}\n`;
    const removedTail = transcript.slice(Buffer.byteLength(preservedPrefix, "utf8"));
    await writeFile(store.messagesPath, transcript);

    const result = await store.frontCompactFromMessage(
      `session-user-o${firstRemovedOffset}`,
      "recoverable",
    );

    assert.equal(result.delete_mode, "recoverable");
    assert.match(result.archive_name ?? "", /^messages-\d{8}T\d{9}Z-[0-9a-f]{8}\.jsonl$/);
    assert.equal(await readFile(store.messagesPath, "utf8"), preservedPrefix);
    assert.equal(
      await readFile(join(store.deletedHistoryDir, result.archive_name!), "utf8"),
      removedTail,
    );
  } finally {
    await store?.close();
    await removeSession(sessionDir);
  }
});

test("front compaction does not replay deleted active records from the compaction cache", async () => {
  const sessionDir = await makeSession();
  let store: SessionResponseStore | undefined;
  try {
    store = await SessionResponseStore.open(sessionDir);
    const user = (text: string): Record<string, unknown> => ({
      type: "message",
      role: "user",
      content: [{type: "input_text", text}],
    });
    const records = [
      user("kept active turn"),
      completedEvent([
        {type: "message", role: "assistant", content: [{type: "output_text", text: "kept answer"}]},
      ], "kept-response"),
      user("deleted active turn"),
      completedEvent([
        {type: "message", role: "assistant", content: [{type: "output_text", text: "deleted answer"}]},
      ], "deleted-response"),
    ];
    const transcript = `${records.map(record => JSON.stringify(record)).join("\n")}\n`;
    const boundary = Buffer.byteLength(`${records.slice(0, 2).map(record => JSON.stringify(record)).join("\n")}\n`, "utf8");
    await writeFile(store.messagesPath, transcript);
    await writeFile(store.compactionCachePath, JSON.stringify({
      schema_version: 1,
      source_sha256: createHash("sha256").update(transcript).digest("hex"),
      items: [{type: "message", role: "user", content: [{type: "input_text", text: "older compacted context"}]}],
      assistant_final_items: [],
      active_tail: records.slice(2),
    }));

    await store.frontCompactFromMessage(`session-user-o${boundary}`, "permanent");
    const requestItems: unknown[] = [];
    for await (const item of store.readInputItems()) requestItems.push(item);
    const requestHistory = JSON.stringify(requestItems);
    assert.match(requestHistory, /older compacted context/);
    assert.match(requestHistory, /kept active turn/);
    assert.doesNotMatch(requestHistory, /deleted active turn|deleted answer/);
  } finally {
    await store?.close();
    await removeSession(sessionDir);
  }
});

test("stages completed items and commits one canonical response event", async () => {
  const sessionDir = await makeSession();
  try {
    const store = await SessionResponseStore.open(sessionDir, { durability: "strict" });
    const turn = await store.beginTurn("turn-out-of-order");
    const messageItem = {
      type: "message",
      role: "assistant",
      content: [{ type: "output_text", text: "Hello" }],
    };
    const functionItem = {
      type: "function_call",
      call_id: "call_123",
      name: "search",
      arguments: "{}",
    };

    await turn.stageOutputItem({
      type: "response.output_item.done",
      output_index: 1,
      item: functionItem,
    });
    await turn.stageOutputItem({
      type: "response.output_item.done",
      output_index: 0,
      item: messageItem,
    });

    const result = await turn.commit(
      completedEvent([messageItem, functionItem]),
    );
    assert.ok(result.bytes > 0);
    assert.equal(result.already_present, false);

    const raw = await readFile(join(sessionDir, "messages.jsonl"), "utf8");
    const lines = raw.trimEnd().split("\n");
    assert.equal(lines.length, 1);
    assert.equal(raw.endsWith("\n"), true);

    const persisted = JSON.parse(lines[0]!) as ResponseCompletedEvent;
    assert.equal(persisted.type, "response.completed");
    assert.deepEqual(persisted.response.output, [messageItem, functionItem]);
    assert.deepEqual(persisted.response.usage, { input_tokens: 10, output_tokens: 4 });
    assert.equal(raw.includes("output_index"), false);

    const stagingEntries = await readdir(join(sessionDir, "staging"));
    assert.deepEqual(stagingEntries, ["usage-cache.json"]);
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("keeps current and cumulative usage in an incremental cache", async () => {
  const sessionDir = await makeSession();
  const usageCachePath = join(sessionDir, "staging", "usage-cache.json");
  const expected = {
    completed_responses: 3,
    responses_with_usage: 3,
    turn_count: 2,
    history_partial: false,
    input_tokens: 152,
    output_tokens: 33,
    total_tokens: 185,
    cached_input_tokens: 10,
    current: {
      input_tokens: 12,
      output_tokens: 3,
      total_tokens: 15,
      turn: 2,
      step: 1,
      response_id: "usage-three",
    },
  };
  try {
    const store = await SessionResponseStore.open(sessionDir);
    const user = (text: string) => ({
      type: "message",
      role: "user",
      content: [{type: "input_text", text}],
    });
    await store.appendInputItem(user("first turn"));

    const first = await store.beginTurn("usage-first");
    await first.commit(usageEvent([], "usage-one", {
      input_tokens: 100,
      output_tokens: 25,
      total_tokens: 125,
      input_tokens_details: {cached_tokens: 10},
    }, 1, 1));
    const second = await store.beginTurn("usage-second");
    await second.commit(usageEvent([], "usage-two", {
      input_tokens: 40,
      output_tokens: 5,
    }, 1, 2));

    await store.appendInputItems([user("second turn part one"), user("second turn part two")]);
    const third = await store.beginTurn("usage-third");
    await third.commit(usageEvent([], "usage-three", {
      input_tokens: 12,
      output_tokens: 3,
    }, 2, 1));

    assert.deepEqual(await store.readUsageSummary(), expected);
    const cache = JSON.parse(await readFile(usageCachePath, "utf8")) as {
      transcript: {offset: number};
      summary: unknown;
    };
    assert.deepEqual(cache.summary, expected);
    assert.equal(cache.transcript.offset, (await readFile(store.messagesPath)).byteLength);
    await store.close();

    // Rebuilding from JSONL preserves the boundary between one multi-item
    // submission and the next session turn.
    await rm(usageCachePath);
    const rebuilt = await SessionResponseStore.open(sessionDir);
    assert.deepEqual(await rebuilt.readUsageSummary(), expected);
    await rebuilt.close();

    // The cache-only reader must not need to open or walk the transcript.
    await rm(store.messagesPath);
    assert.deepEqual(await SessionResponseStore.readCachedUsageSummary(usageCachePath), expected);
  } finally {
    await removeSession(sessionDir);
  }
});

test("compacts provider usage details and rebuilds the usage cache from retained counters", async () => {
  const sessionDir = await makeSession();
  const usageCachePath = join(sessionDir, "staging", "usage-cache.json");
  try {
    const store = await SessionResponseStore.open(sessionDir);
    const turn = await store.beginTurn("usage-compact");
    await turn.commit(usageEvent([], "usage-compact-response", {
      input_tokens: 20,
      output_tokens: 5,
      total_tokens: 25,
      input_tokens_details: {cached_tokens: 4, cache_write_tokens: 7},
      output_tokens_details: {reasoning_tokens: 3},
      attribution: {items: {"fc_provider-item": {input_tokens: 12}}},
    }, 1, 1));

    const raw = await readFile(store.messagesPath, "utf8");
    const persisted = JSON.parse(raw.trimEnd()) as ResponseCompletedEvent;
    assert.deepEqual(persisted.response.usage, {
      input_tokens: 20,
      output_tokens: 5,
      total_tokens: 25,
      input_tokens_details: {cached_tokens: 4},
    });
    assert.doesNotMatch(raw, /attribution|cache_write_tokens|reasoning_tokens/);
    const summary = await store.readUsageSummary();
    assert.equal(summary.cached_input_tokens, 4);
    assert.equal(summary.total_tokens, 25);
    await store.close();

    await rm(usageCachePath, {force: true});
    const rebuilt = await SessionResponseStore.open(sessionDir);
    assert.deepEqual(await rebuilt.readUsageSummary(), summary);
    await rebuilt.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("recovers an uncheckpointed usage transcript append without double-counting", async () => {
  const sessionDir = await makeSession();
  const user = {type: "message", role: "user", content: [{type: "input_text", text: "first"}]};
  try {
    const store = await SessionResponseStore.open(sessionDir);
    await store.appendInputItem(user);
    const first = await store.beginTurn("usage-recovery-first");
    await first.commit(usageEvent([], "usage-recovery-one", {
      input_tokens: 20,
      output_tokens: 8,
    }, 1, 1));
    const checkpoint = JSON.parse(await readFile(store.usageCachePath, "utf8")) as {
      transcript: {offset: number};
    };

    const secondUser = {type: "message", role: "user", content: [{type: "input_text", text: "second"}]};
    const secondEvent = usageEvent([], "usage-recovery-two", {
      input_tokens: 15,
      output_tokens: 5,
    }, 2, 1);
    await appendFile(
      store.messagesPath,
      `${JSON.stringify(secondUser)}\n${JSON.stringify(secondEvent)}\n`,
    );
    const recovered = await store.readUsageSummary();
    assert.equal(recovered.completed_responses, 2);
    assert.equal(recovered.responses_with_usage, 2);
    assert.equal(recovered.turn_count, 2);
    assert.equal(recovered.input_tokens, 35);
    assert.equal(recovered.output_tokens, 13);
    assert.equal(recovered.total_tokens, 48);
    assert.deepEqual(recovered.current, {
      input_tokens: 15,
      output_tokens: 5,
      total_tokens: 20,
      turn: 2,
      step: 1,
      response_id: "usage-recovery-two",
    });
    assert.ok(checkpoint.transcript.offset < (await readFile(store.messagesPath)).byteLength);
    assert.deepEqual(await store.readUsageSummary(), recovered);
    await store.close();

    const reopened = await SessionResponseStore.open(sessionDir);
    assert.deepEqual(await reopened.readUsageSummary(), recovered);
    await reopened.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("refreshes uncheckpointed usage before compaction moves the transcript checkpoint", async () => {
  const sessionDir = await makeSession();
  try {
    const store = await SessionResponseStore.open(sessionDir);
    const user = {type: "message", role: "user", content: [{type: "input_text", text: "compact this"}]};
    const assistant = {type: "message", role: "assistant", content: [{type: "output_text", text: "answer"}]};
    await store.appendInputItem(user);
    const first = await store.beginTurn("usage-compact-first");
    await first.stageOutputItem({type: "response.output_item.done", output_index: 0, item: assistant});
    await first.commit(usageEvent([assistant], "usage-compact-one", {
      input_tokens: 30,
      output_tokens: 8,
    }, 1, 1));
    await store.appendInputItem({
      type: "function_call_output",
      call_id: "large-compact-output",
      output: "tool data ".repeat(500),
    });

    const uncheckpointed = usageEvent([], "usage-compact-two", {
      input_tokens: 12,
      output_tokens: 4,
    }, 1, 2);
    await appendFile(store.messagesPath, `${JSON.stringify(uncheckpointed)}\n`);
    const compacted = await store.compactHistory({
      turnType: "full",
      keepTurns: 0,
      keepToolPairs: 0,
      backupMode: "replace",
    });
    assert.equal(compacted.compacted, true);
    assert.equal(await readFile(store.messagesPath, "utf8"), "");
    const compactedHistory = [];
    for await (const item of store.readInputItems()) compactedHistory.push(item);
    assert.deepEqual(compactedHistory, []);
    assert.deepEqual(await store.readUsageSummary(), {
      completed_responses: 2,
      responses_with_usage: 2,
      turn_count: 1,
      history_partial: false,
      input_tokens: 42,
      output_tokens: 12,
      total_tokens: 54,
      current: {
        input_tokens: 0,
        output_tokens: 0,
        total_tokens: 0,
        turn: 1,
        step: 2,
        response_id: "usage-compact-two",
        cached_input_tokens: 0,
      },
    });
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("stores raw input items and reconstructs request history", async () => {
  const sessionDir = await makeSession();
  try {
    const store = await SessionResponseStore.open(sessionDir);
    const userItem = {
      type: "message",
      role: "user",
      content: [{ type: "input_text", text: "Find the answer" }],
    };
    const functionCall = {
      type: "function_call",
      call_id: "call_history",
      name: "lookup",
      arguments: "{}",
    };
    const toolOutput = {
      type: "function_call_output",
      call_id: "call_history",
      output: "answer",
    };

    await store.appendInputItem(userItem);
    const turn = await store.beginTurn("turn-history");
    await turn.stageOutputItem({
      type: "response.output_item.done",
      output_index: 0,
      item: functionCall,
    });
    await turn.commit(completedEvent([functionCall], "resp_history"));
    await store.appendInputItem(toolOutput);

    const history = [];
    for await (const item of store.readInputItems()) history.push(item);
    assert.deepEqual(history, [userItem, functionCall, toolOutput]);

    const lines = (await readFile(join(sessionDir, "messages.jsonl"), "utf8"))
      .trimEnd()
      .split("\n");
    assert.equal(JSON.parse(lines[0]!).type, "message");
    assert.equal(JSON.parse(lines[1]!).type, "response.completed");
    assert.equal(JSON.parse(lines[2]!).type, "function_call_output");
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("reads JSONL records containing literal U+2028 and U+2029 characters", async () => {
  const sessionDir = await makeSession();
  try {
    const store = await SessionResponseStore.open(sessionDir);
    const text = "before\u2028middle\u2029after";
    const toolOutput = {
      type: "function_call_output",
      call_id: "call_unicode_separators",
      output: text,
    };
    await store.appendInputItem(toolOutput);

    const history: unknown[] = [];
    for await (const item of store.readInputItems()) history.push(item);
    assert.deepEqual(history, [toolOutput]);

    const assistantMessage = {
      type: "message",
      role: "assistant",
      content: [{type: "output_text", text}],
    };
    const turn = await store.beginTurn("unicode-separators");
    await turn.stageOutputItem({
      type: "response.output_item.done",
      output_index: 0,
      item: assistantMessage,
    });
    await turn.commit(completedEvent([assistantMessage], "resp_unicode_separators"));

    const completions: ResponseCompletedEvent[] = [];
    for await (const event of store.readCompletedResponses()) completions.push(event);
    assert.equal(completions.length, 1);
    assert.deepEqual(completions[0]?.response.output, [assistantMessage]);
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("writes normalized history to its configured cache path", async () => {
  const sessionDir = await makeSession();
  const cachePath = join(sessionDir, "separate-cache", "history.json");
  try {
    const store = await SessionResponseStore.open(sessionDir, {historyCachePath: "separate-cache/history.json"});
    assert.equal(store.historyCachePath, cachePath);
    const item = {type: "message", role: "user", content: [{type: "input_text", text: "cache me"}]};
    await store.appendInputItem(item);
    const history = [];
    for await (const input of store.readInputItems()) history.push(input);
    assert.deepEqual(history, [item]);
    const cache = JSON.parse(await readFile(cachePath, "utf8")) as {items: unknown[]};
    assert.deepEqual(cache.items, [item]);
    await assert.rejects(() => readFile(join(sessionDir, "staging/history-cache.json"), "utf8"), {code: "ENOENT"});
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("persists normalized request history and refreshes only appended transcript records", async () => {
  const sessionDir = await makeSession();
  try {
    const store = await SessionResponseStore.open(sessionDir);
    const first = {
      type: "message",
      role: "user",
      content: [{type: "input_text", text: "first"}],
    };
    const second = {
      type: "message",
      role: "user",
      content: [{type: "input_text", text: "second"}],
    };
    await store.appendInputItem(first);
    const firstHistory = [];
    for await (const item of store.readInputItems()) firstHistory.push(item);
    assert.deepEqual(firstHistory, [first]);

    const firstCache = JSON.parse(
      await readFile(join(sessionDir, "staging", "history-cache.json"), "utf8"),
    ) as {schema_version: number; transcript: {offset: number}; items: unknown[]};
    assert.equal(firstCache.schema_version, 1);
    assert.equal(firstCache.transcript.offset, (await readFile(join(sessionDir, "messages.jsonl"))).length);
    assert.deepEqual(firstCache.items, [first]);

    await store.appendInputItem(second);
    const secondHistory = [];
    for await (const item of store.readInputItems()) secondHistory.push(item);
    assert.deepEqual(secondHistory, [first, second]);
    const secondCache = JSON.parse(
      await readFile(join(sessionDir, "staging", "history-cache.json"), "utf8"),
    ) as {transcript: {offset: number}; items: unknown[]};
    assert.equal(secondCache.transcript.offset, (await readFile(join(sessionDir, "messages.jsonl"))).length);
    assert.deepEqual(secondCache.items, [first, second]);
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("defers request history-cache writes and recovers deferred items from the transcript", async () => {
  const sessionDir = await makeSession();
  let store: SessionResponseStore | undefined;
  try {
    store = await SessionResponseStore.open(sessionDir);
    const first = {
      type: "message",
      role: "user",
      content: [{type: "input_text", text: "first"}],
    };
    const second = {
      type: "message",
      role: "user",
      content: [{type: "input_text", text: "second"}],
    };
    const third = {
      type: "message",
      role: "user",
      content: [{type: "input_text", text: "third"}],
    };
    const cachePath = join(sessionDir, "staging", "history-cache.json");
    const messagesPath = join(sessionDir, "messages.jsonl");

    await store.appendInputItem(first);
    const initialHistory = [];
    for await (const item of store.readInputItems()) initialHistory.push(item);
    assert.deepEqual(initialHistory, [first]);
    const initialCache = await readFile(cachePath, "utf8");

    await store.appendInputItem(second);
    const secondTranscript = await readFile(messagesPath);
    const deferredHistory = [];
    for await (const item of store.readInputItems({persistCache: false})) deferredHistory.push(item);
    assert.deepEqual(deferredHistory, [first, second]);
    assert.deepEqual(await readFile(messagesPath), secondTranscript);
    assert.equal(await readFile(cachePath, "utf8"), initialCache);

    const persistedHistory = [];
    for await (const item of store.readInputItems()) persistedHistory.push(item);
    assert.deepEqual(persistedHistory, [first, second]);
    const secondCache = await readFile(cachePath, "utf8");
    assert.deepEqual((JSON.parse(secondCache) as {items: unknown[]}).items, [first, second]);

    await store.appendInputItem(third);
    const thirdTranscript = await readFile(messagesPath);
    const latestHistory = [];
    for await (const item of store.readInputItems({persistCache: false})) latestHistory.push(item);
    assert.deepEqual(latestHistory, [first, second, third]);
    assert.deepEqual(await readFile(messagesPath), thirdTranscript);
    assert.equal(await readFile(cachePath, "utf8"), secondCache);
    await store.close();
    store = await SessionResponseStore.open(sessionDir);

    const recoveredHistory = [];
    for await (const item of store.readInputItems()) recoveredHistory.push(item);
    assert.deepEqual(recoveredHistory, [first, second, third]);
    const recoveredCache = JSON.parse(await readFile(cachePath, "utf8")) as {
      transcript: {offset: number};
      items: unknown[];
    };
    assert.deepEqual(recoveredCache.items, [first, second, third]);
    assert.equal(recoveredCache.transcript.offset, (await readFile(join(sessionDir, "messages.jsonl"))).length);
  } finally {
    await store?.close();
    await removeSession(sessionDir);
  }
});

test("rebuilds cached history when the transcript prefix changes", async () => {
  const sessionDir = await makeSession();
  try {
    const store = await SessionResponseStore.open(sessionDir);
    const original = {
      type: "message",
      role: "user",
      content: [{type: "input_text", text: "original"}],
    };
    const replacement = {
      type: "message",
      role: "user",
      content: [{type: "input_text", text: "a substantially longer replacement"}],
    };
    await store.appendInputItem(original);
    for await (const _item of store.readInputItems()) {
      // Populate the staged cache before replacing the authoritative transcript.
    }
    await writeFile(join(sessionDir, "messages.jsonl"), `${JSON.stringify(replacement)}\n`);
    const history = [];
    for await (const item of store.readInputItems()) history.push(item);
    assert.deepEqual(history, [replacement]);
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("uses fallback sequence numbers when output_index is absent", async () => {
  const sessionDir = await makeSession();
  try {
    const store = await SessionResponseStore.open(sessionDir);
    const turn = await store.beginTurn("turn-sequence");
    const first = { type: "message", role: "assistant", content: [] };
    const second = { type: "message", role: "assistant", content: [] };

    await turn.stageOutputItem({ type: "response.output_item.done", item: first });
    await turn.stageOutputItem({ type: "response.output_item.done", item: second });
    await turn.commit(completedEvent([first, second], "resp_sequence"));

    const [line] = (await readFile(join(sessionDir, "messages.jsonl"), "utf8"))
      .trimEnd()
      .split("\n");
    const persisted = JSON.parse(line!) as ResponseCompletedEvent;
    assert.deepEqual(persisted.response.output, [first, second]);
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("repairs an incomplete messages.jsonl trailing record on open", async () => {
  const sessionDir = await makeSession();
  try {
    await mkdir(sessionDir, { recursive: true });
    await writeFile(
      join(sessionDir, "messages.jsonl"),
      '{"type":"response.completed","response":{"id":"resp_ok"}}\n{"type":"response.completed"',
    );

    const store = await SessionResponseStore.open(sessionDir);
    const raw = await readFile(join(sessionDir, "messages.jsonl"), "utf8");
    assert.equal(raw, '{"type":"response.completed","response":{"id":"resp_ok"}}\n');
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("retains a streaming turn but removes a partial staging line during recovery", async () => {
  const sessionDir = await makeSession();
  try {
    const store = await SessionResponseStore.open(sessionDir);
    const turn = await store.beginTurn("turn-partial-stage");
    await turn.stageOutputItem({
      type: "response.output_item.done",
      output_index: 0,
      item: { type: "message", role: "assistant", content: [] },
    });
    await turn.close();

    const itemsPath = join(sessionDir, "staging", "turn-partial-stage", "items.jsonl");
    await appendFile(itemsPath, '{"output_index":1,"item":{"type":"message"');
    await store.close();

    const reopened = await SessionResponseStore.open(sessionDir);
    const report = await reopened.recover();
    assert.equal(report.inspected_turns, 1);
    assert.equal(report.retained_turns, 1);
    assert.deepEqual(report.errors, []);

    const repaired = await readFile(itemsPath, "utf8");
    assert.equal(repaired.endsWith("\n"), true);
    assert.equal(repaired.includes('"output_index":1'), false);
    await reopened.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("cleans only dead-owner partial caches when opening a response store", async () => {
  const sessionDir = await makeSession();
  let store: SessionResponseStore | undefined;
  try {
    const child = spawn(process.execPath, ["-e", "process.exit(0)"], {stdio: "ignore"});
    const deadPid = child.pid;
    assert.ok(deadPid);
    await new Promise<void>((resolve, reject) => {
      child.once("error", reject);
      child.once("close", () => resolve());
    });
    assert.throws(() => process.kill(deadPid, 0), (error: NodeJS.ErrnoException) => error.code === "ESRCH");

    const staging = join(sessionDir, "staging");
    const orphan = join(staging, "turn-cache-orphan");
    const live = join(staging, "turn-cache-live");
    await mkdir(orphan, {recursive: true});
    await mkdir(live, {recursive: true});
    await writeFile(join(orphan, "manifest.json"), JSON.stringify({status: "streaming"}));
    await writeFile(join(orphan, "items.jsonl"), "staged provider item remains\n");
    await writeFile(join(orphan, RESPONSE_PARTIAL_FILE), Buffer.from("orphan", "utf16le"));
    await writeFile(join(orphan, RESPONSE_PARTIAL_COMPACT_FILE), Buffer.from("temporary", "utf16le"));
    await writeFile(join(orphan, RESPONSE_PARTIAL_OWNER_FILE), JSON.stringify({hostname: hostname(), pid: deadPid}));
    await writeFile(join(orphan, RESPONSE_PARTIAL_CLEANUP_LOCK_FILE),
      JSON.stringify({hostname: hostname(), pid: deadPid, token: "stale-cleanup"}));
    await writeFile(join(live, "manifest.json"), JSON.stringify({status: "streaming"}));
    await writeFile(join(live, RESPONSE_PARTIAL_FILE), Buffer.from("live", "utf16le"));
    await writeFile(join(live, RESPONSE_PARTIAL_OWNER_FILE), JSON.stringify({hostname: hostname(), pid: process.pid}));

    store = await SessionResponseStore.open(sessionDir);
    await assert.rejects(readFile(join(orphan, RESPONSE_PARTIAL_FILE)), {code: "ENOENT"});
    await assert.rejects(readFile(join(orphan, RESPONSE_PARTIAL_COMPACT_FILE)), {code: "ENOENT"});
    await assert.rejects(readFile(join(orphan, RESPONSE_PARTIAL_OWNER_FILE)), {code: "ENOENT"});
    await assert.rejects(readFile(join(orphan, RESPONSE_PARTIAL_CLEANUP_LOCK_FILE)), {code: "ENOENT"});
    assert.equal(await readFile(join(orphan, "items.jsonl"), "utf8"), "staged provider item remains\n");
    assert.equal((await readFile(join(live, RESPONSE_PARTIAL_FILE))).toString("utf16le"), "live");
    assert.equal(await readFile(join(live, RESPONSE_PARTIAL_OWNER_FILE), "utf8"),
      JSON.stringify({hostname: hostname(), pid: process.pid}));
  } finally {
    await store?.close();
    await removeSession(sessionDir);
  }
});

test("recovers a rendered final record after a commit interruption", async () => {
  const sessionDir = await makeSession();
  try {
    const turnDir = join(sessionDir, "staging", "turn-recover-commit");
    await mkdir(turnDir, { recursive: true });
    const finalLine =
      '{"type":"response.completed","response":{"id":"resp_recover","status":"completed","output":[]}}\n';
    const hash = createHash("sha256").update(finalLine).digest("hex");
    await writeFile(join(turnDir, "manifest.json"), `${JSON.stringify({
      schema_version: 1,
      turn_id: "turn-recover-commit",
      response_id: "resp_recover",
      status: "committing",
      item_count: 0,
      next_sequence: 0,
      canonical_line_sha256: hash,
    })}\n`);
    await writeFile(join(turnDir, "items.jsonl"), "");
    await writeFile(join(turnDir, "items.index.jsonl"), "");
    await writeFile(join(turnDir, "final.jsonl"), finalLine);

    const store = await SessionResponseStore.open(sessionDir);
    const report = await store.recover();
    assert.equal(report.committed_turns, 1);
    assert.equal(report.removed_turns, 1);
    assert.deepEqual(report.errors, []);
    assert.equal(await readFile(join(sessionDir, "messages.jsonl"), "utf8"), finalLine);
    assert.deepEqual(await readdir(join(sessionDir, "staging")), ["usage-cache.json"]);
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("stores only the response fields the transcript reads, without the provider request echo", async () => {
  const sessionDir = await makeSession();
  try {
    const store = await SessionResponseStore.open(sessionDir);
    assert.equal(store.responseMetadata, "minimal");
    const item = {type: "message", role: "assistant", content: [{type: "output_text", text: "answer"}]};
    const turn = await store.beginTurn("turn-metadata-minimal");
    await turn.stageOutputItem({type: "response.output_item.done", output_index: 0, item});
    const event = completedEvent([item], "resp-metadata");
    event.response = {
      ...event.response,
      tools: [{type: "function", name: "read", description: "request echo ".repeat(500)}],
      tool_choice: "auto",
      truncation: "disabled",
      service_tier: "auto",
      text: {format: {type: "text"}},
      reasoning: {effort: "max", summary: "auto"},
      prompt_cache_key: "jad-echo",
      created_at: 1790186909,
      completed_at: 1790186915,
    };
    await turn.commit(event);

    const raw = await readFile(store.messagesPath, "utf8");
    const [line] = raw.trimEnd().split("\n");
    const persisted = JSON.parse(line!) as ResponseCompletedEvent;
    assert.deepEqual(persisted.response, {
      id: "resp-metadata",
      object: "response",
      status: "completed",
      model: "gpt-test",
      created_at: 1790186909,
      completed_at: 1790186915,
      usage: {input_tokens: 10, output_tokens: 4},
      output: [item],
    });
    assert.doesNotMatch(raw, /request echo/);

    // The request history and the usage summary are unchanged by the trim.
    const history = [];
    for await (const input of store.readInputItems()) history.push(input);
    assert.deepEqual(history, [item]);
    const usage = await store.readUsageSummary();
    assert.equal(usage.current?.response_id, "resp-metadata");
    assert.equal(usage.current?.input_tokens, 10);
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("full response metadata preserves provider fields but compacts usage", async () => {
  const sessionDir = await makeSession();
  try {
    const store = await SessionResponseStore.open(sessionDir, {responseMetadata: "full"});
    assert.equal(store.responseMetadata, "full");
    const item = {type: "message", role: "assistant", content: [{type: "output_text", text: "answer"}]};
    const turn = await store.beginTurn("turn-metadata-full");
    await turn.stageOutputItem({type: "response.output_item.done", output_index: 0, item});
    const event = completedEvent([item], "resp-metadata-full");
    event.response = {
      ...event.response,
      tools: [{type: "function", name: "read"}],
      usage: {
        input_tokens: 10,
        output_tokens: 4,
        total_tokens: 14,
        input_tokens_details: {cached_tokens: 2, cache_write_tokens: 99},
        output_tokens_details: {reasoning_tokens: 3},
        attribution: {items: {"fc_provider-item": {input_tokens: 5}}},
      },
    };
    await turn.commit(event);

    const raw = await readFile(store.messagesPath, "utf8");
    const [line] = raw.trimEnd().split("\n");
    const persisted = JSON.parse(line!) as ResponseCompletedEvent;
    assert.deepEqual(persisted.response.tools, [{type: "function", name: "read"}]);
    assert.deepEqual(persisted.response.usage, {
      input_tokens: 10,
      output_tokens: 4,
      total_tokens: 14,
      input_tokens_details: {cached_tokens: 2},
    });
    assert.doesNotMatch(raw, /attribution|cache_write_tokens|reasoning_tokens/);
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("rejects an unsupported persistence.response_metadata and threads a configured one", async () => {
  const sessionDir = await makeSession();
  try {
    await writeFile(join(sessionDir, "model.json"), JSON.stringify({model: "gpt-test"}));
    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({cwd: sessionDir, persistence: {response_metadata: "tiny"}}),
    );
    await assert.rejects(
      () => new SessionConfigLoader(sessionDir).loadSettings(),
      /persistence\.response_metadata must be minimal or full/,
    );

    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({cwd: sessionDir, persistence: {response_metadata: "full"}}),
    );
    const config = await new SessionConfigLoader(sessionDir).load();
    assert.equal(config.settings.persistence.response_metadata, "full");
    const session = await AgentSession.open(sessionDir);
    assert.equal(session.store.responseMetadata, "full");
    await session.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("validates the run-error cache entry cap and defaults it", async () => {
  const sessionDir = await makeSession();
  try {
    await writeFile(join(sessionDir, "model.json"), JSON.stringify({model: "gpt-test"}));
    for (const value of [0, MAX_RUN_ERROR_CACHE_MAX_ENTRIES + 1, 1.5, "50"]) {
      await writeFile(
        join(sessionDir, "setting.json"),
        JSON.stringify({cwd: sessionDir, persistence: {run_error_cache_max_entries: value}}),
      );
      await assert.rejects(
        () => new SessionConfigLoader(sessionDir).loadSettings(),
        /persistence\.run_error_cache_max_entries must be a safe integer/,
        `value ${JSON.stringify(value)} must be rejected`,
      );
    }

    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({cwd: sessionDir, persistence: {run_error_cache_max_entries: 3}}),
    );
    assert.equal((await new SessionConfigLoader(sessionDir).load()).settings.persistence.run_error_cache_max_entries, 3);

    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({cwd: sessionDir}));
    assert.equal(
      (await new SessionConfigLoader(sessionDir).load()).settings.persistence.run_error_cache_max_entries,
      DEFAULT_RUN_ERROR_CACHE_MAX_ENTRIES,
    );
  } finally {
    await removeSession(sessionDir);
  }
});

test("validates and threads the cursor-cache checkpoint byte threshold", async () => {
  const sessionDir = await makeSession();
  try {
    await writeFile(join(sessionDir, "model.json"), JSON.stringify({model: "gpt-test"}));
    for (const value of [0, MAX_CURSOR_CACHE_CHECKPOINT_BYTES + 1, 1.5, "1024"]) {
      await writeFile(
        join(sessionDir, "setting.json"),
        JSON.stringify({cwd: sessionDir, persistence: {cursor_cache_checkpoint_bytes: value}}),
      );
      await assert.rejects(
        () => new SessionConfigLoader(sessionDir).loadSettings(),
        /persistence\.cursor_cache_checkpoint_bytes must be a safe integer/,
      );
    }

    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({cwd: sessionDir, persistence: {cursor_cache_checkpoint_bytes: 1}}),
    );
    const config = await new SessionConfigLoader(sessionDir).load();
    assert.equal(config.settings.persistence.cursor_cache_checkpoint_bytes, 1);

    const session = await AgentSession.open(sessionDir);
    try {
      await session.store.readCursorIndexSnapshot();
      const completed = {
        type: "response.completed",
        session_turn: 1,
        session_step: 1,
        response: {id: "checkpoint-run", status: "completed", output: []},
      };
      await appendFile(session.store.messagesPath, `${JSON.stringify(completed)}\n`);
      const index = await session.store.readCursorIndexSnapshot();
      assert.equal(index.runs.length, 1);

      const cached = JSON.parse(await readFile(session.store.cursorCachePath, "utf8")) as {
        transcript: {offset: number};
      };
      assert.equal(
        cached.transcript.offset,
        Buffer.byteLength(await readFile(session.store.messagesPath, "utf8")),
      );
    } finally {
      await session.close();
    }
  } finally {
    await removeSession(sessionDir);
  }
});

test("rejects incomplete output sets and oversized items without committing", async () => {
  const sessionDir = await makeSession();
  try {
    const store = await SessionResponseStore.open(sessionDir, { maxItemBytes: 64 });
    const turn = await store.beginTurn("turn-validation");
    await assert.rejects(
      () =>
        turn.stageOutputItem({
          type: "response.output_item.done",
          output_index: 0,
          item: { type: "message", content: [{ type: "output_text", text: "x".repeat(128) }] },
        }),
      (error: unknown) => error instanceof PersistenceError,
    );

    await turn.stageOutputItem({
      type: "response.output_item.done",
      output_index: 0,
      item: { type: "message", role: "assistant", content: [] },
    });
    await assert.rejects(
      () => turn.commit(completedEvent([{ type: "message" }, { type: "message" }], "resp_bad")),
      (error: unknown) => error instanceof PersistenceError,
    );
    assert.equal((await readFile(join(sessionDir, "messages.jsonl"), "utf8")), "");
    await turn.close();
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("reconstructs empty terminal output from out-of-order staged stream items", async () => {
  const sessionDir = await makeSession();
  try {
    const output = [
      { type: "reasoning", id: "rsn_adapter", summary: [] },
      { type: "message", role: "assistant", content: [] },
    ];
    async function* events(): AsyncGenerator<unknown> {
      yield {
        type: "response.output_item.done",
        output_index: 1,
        item: output[1],
      };
      yield {
        type: "response.output_item.done",
        output_index: 0,
        item: output[0],
      };
      yield completedEvent([], "resp_adapter");
    }

    const store = await SessionResponseStore.open(sessionDir);
    const result = await persistResponseStream(store, events(), { turnId: "turn-adapter" });
    assert.equal(result.already_present, false);
    assert.deepEqual(result.event.response.output, output);
    const records = [];
    for await (const record of store.readCompletedResponses()) {
      records.push(record);
    }
    assert.equal(records.length, 1);
    assert.equal(records[0]!.response.id, "resp_adapter");
    assert.deepEqual(records[0]!.response.output, output);
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("commits completed output items when a provider response is incomplete", async () => {
  const sessionDir = await makeSession();
  try {
    const functionCall = {
      type: "function_call",
      call_id: "call_incomplete",
      name: "read",
      arguments: JSON.stringify({path: "sample.txt"}),
    };
    async function* events(): AsyncGenerator<unknown> {
      yield {
        type: "response.output_item.done",
        output_index: 0,
        item: functionCall,
      };
      yield {
        type: "response.incomplete",
        response: {
          id: "resp_incomplete",
          object: "response",
          status: "incomplete",
          incomplete_details: {reason: "max_output_tokens"},
          output: [functionCall],
        },
      };
    }

    const store = await SessionResponseStore.open(sessionDir);
    const result = await persistResponseStream(store, events(), {turnId: "turn-incomplete"});
    assert.equal(result.event.type, "response.completed");
    assert.equal(result.event.response.status, "incomplete");
    assert.deepEqual(result.event.response.incomplete_details, {reason: "max_output_tokens"});

    const history = [];
    for await (const item of store.readInputItems()) {
      history.push(item);
    }
    assert.deepEqual(history, [functionCall]);
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("loads model and session settings with normalized paths and hashes", async () => {
  const sessionDir = await makeSession();
  try {
    await writeFile(
      join(sessionDir, "model.json"),
      JSON.stringify({
        request: { max_output_tokens: 1024 },
        model: "inclusionai/ling-3.0-flash-fin:free",
        base_url: "https://openrouter.ai/api/v1/",
        provider: "openrouter",
        api_key_env: "OPENROUTER_API_KEY",
      }),
    );
    await writeFile(join(sessionDir, "base.md"), "Be concise.");
    const absolutePrompt = join(sessionDir, "product.md");
    await writeFile(absolutePrompt, "Use tools.");
    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({
        prompt_documents: ["base.md", absolutePrompt],
        prompt_role: "developer",
        cwd: sessionDir,
        session_name: "config-test",
        mcp_servers: [
          {id: "docs", config_path: "mcp/docs.json", enabled: true, deny_tools: ["delete"]},
          {id: "optional", config_path: absolutePrompt},
        ],
      }),
    );

    const config = await new SessionConfigLoader(sessionDir).load();
    assert.equal(config.model.schema_version, 1);
    assert.equal(config.model.base_url, "https://openrouter.ai/api/v1");
    assert.equal(config.settings.cwd, sessionDir);
    assert.deepEqual(config.settings.prompt_documents, [join(sessionDir, "base.md"), absolutePrompt]);
    assert.equal(config.settings.prompt_role, "developer");
    assert.equal(config.settings.prompt_text, "Be concise.\n\nUse tools.");
    assert.deepEqual(config.settings.mcp_servers, [
      {id: "docs", config_path: join(sessionDir, "mcp/docs.json"), enabled: true, deny_tools: ["delete"]},
      {id: "optional", config_path: absolutePrompt, enabled: false, deny_tools: []},
    ]);
    assert.equal(config.settings.max_tool_rounds, 8);
    assert.deepEqual(config.settings.compaction, {
      auto_enabled: true,
      trigger_tokens: 200000,
      keepbatch: {tools: 0, turn: {type: "assistant_final", keep: 1}},
      keepbackup: {turns: 20},
      backup_mode: "append",
    });
    assert.deepEqual(config.settings.persistence, {
      durability: "turn",
      max_item_bytes: 10 * 1024 * 1024,
      response_metadata: "minimal",
      cursor_cache_checkpoint_bytes: 1024 * 1024,
      run_error_cache_max_entries: DEFAULT_RUN_ERROR_CACHE_MAX_ENTRIES,
    });
    assert.deepEqual(config.settings.http_debug, {
      enabled: false,
      include_request_headers: false,
      include_request_body: false,
      include_response_headers: true,
      include_response_body: false,
      max_body_bytes: 65536,
    });
    assert.match(config.model_sha256, /^[a-f0-9]{64}$/);
    assert.match(config.settings_sha256, /^[a-f0-9]{64}$/);
    assert.equal(resolveApiKey(config.model, { OPENROUTER_API_KEY: "secret" }), "secret");
    assert.equal(sha256Json({ b: 2, a: 1 }), sha256Json({ a: 1, b: 2 }));
  } finally {
    await removeSession(sessionDir);
  }
});

test("validates configurable tool-round limits", async () => {
  const sessionDir = await makeSession();
  try {
    await writeFile(join(sessionDir, "model.json"), JSON.stringify({model: "gpt-test"}));
    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({cwd: sessionDir, max_tool_rounds: 50000}),
    );
    assert.equal((await new SessionConfigLoader(sessionDir).loadSettings()).max_tool_rounds, 50000);

    for (const value of [-1, 50001, 1.5, "12"]) {
      await writeFile(
        join(sessionDir, "setting.json"),
        JSON.stringify({cwd: sessionDir, max_tool_rounds: value}),
      );
      await assert.rejects(
        () => new SessionConfigLoader(sessionDir).loadSettings(),
        (error: unknown) => error instanceof ConfigError && /max_tool_rounds/.test(error.message),
      );
    }
  } finally {
    await removeSession(sessionDir);
  }
});

test("normalizes and validates max_parallel_tool_calls", async () => {
  const sessionDir = await makeSession();
  try {
    await writeFile(join(sessionDir, "model.json"), JSON.stringify({model: "gpt-test"}));
    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({cwd: sessionDir}));
    assert.equal((await new SessionConfigLoader(sessionDir).loadSettings()).max_parallel_tool_calls, 8);

    for (const accepted of [1, 64]) {
      await writeFile(join(sessionDir, "setting.json"), JSON.stringify({cwd: sessionDir, max_parallel_tool_calls: accepted}));
      assert.equal((await new SessionConfigLoader(sessionDir).loadSettings()).max_parallel_tool_calls, accepted);
    }
    for (const value of [0, 65, 1.5, "4"]) {
      await writeFile(join(sessionDir, "setting.json"), JSON.stringify({cwd: sessionDir, max_parallel_tool_calls: value}));
      await assert.rejects(
        () => new SessionConfigLoader(sessionDir).loadSettings(),
        (error: unknown) => error instanceof ConfigError && /max_parallel_tool_calls/.test(error.message),
      );
    }
  } finally {
    await removeSession(sessionDir);
  }
});

test("validates MCP server references and exact deny lists", async () => {
  const sessionDir = await makeSession();
  try {
    for (const mcp_servers of [
      [{id: "bad id", config_path: "server.json"}],
      [{id: "duplicate", config_path: "one.json"}, {id: "duplicate", config_path: "two.json"}],
      [{id: "server", config_path: "server.json", enabled: "yes"}],
      [{id: "server", config_path: "server.json", deny_tools: ["hidden", "hidden"]}],
    ]) {
      await writeFile(join(sessionDir, "setting.json"), JSON.stringify({cwd: sessionDir, mcp_servers}));
      await assert.rejects(() => new SessionConfigLoader(sessionDir).loadSettings(), /mcp_servers/);
    }
  } finally {
    await removeSession(sessionDir);
  }
});

test("normalizes and validates compaction settings", async () => {
  const sessionDir = await makeSession();
  try {
    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({
      compaction: {auto_enabled: false, trigger_tokens: 1234, backup_mode: "append", keepbatch: {tools: 2, turn: {type: "full", keep: 3}}, keepbackup: {turns: 5}},
    }));
    const settings = await new SessionConfigLoader(sessionDir).loadSettings();
    assert.deepEqual(settings.compaction, {
      auto_enabled: false,
      trigger_tokens: 1234,
      backup_mode: "append",
      keepbatch: {tools: 2, turn: {type: "full", keep: 3}},
      keepbackup: {turns: 5},
    });
    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({compaction: {backup_mode: "replace"}}));
    assert.equal((await new SessionConfigLoader(sessionDir).loadSettings()).compaction.backup_mode, "replace");
    for (const compaction of [
      {trigger_tokens: 0},
      {keepbatch: {tools: -1}},
      {keepbatch: {turn: {type: "summary"}}},
      {backup_mode: "archive"},
      {keepbackup: {turns: -1}},
      {keepbackup: {turns: 2000}},
      {keepbackup: 5},
    ]) {
      await writeFile(join(sessionDir, "setting.json"), JSON.stringify({compaction}));
      await assert.rejects(() => new SessionConfigLoader(sessionDir).loadSettings(), /compaction/);
    }
    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({paths: {messages: "same", usage_cache: "same"}}));
    await assert.rejects(() => new SessionConfigLoader(sessionDir).loadSettings(), /must be different files/);
  } finally {
    await removeSession(sessionDir);
  }
});

test("normalizes and validates front-compaction settings", async () => {
  const sessionDir = await makeSession();
  try {
    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({
      front_compaction: {enabled: false, delete_mode: "recoverable"},
    }));
    const settings = await new SessionConfigLoader(sessionDir).loadSettings();
    assert.deepEqual(settings.front_compaction, {enabled: false, delete_mode: "recoverable"});

    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({}));
    assert.deepEqual(
      (await new SessionConfigLoader(sessionDir).loadSettings()).front_compaction,
      {enabled: true, delete_mode: "permanent"},
    );

    for (const front_compaction of [
      {enabled: "yes"},
      {delete_mode: "backup"},
      1,
    ]) {
      await writeFile(join(sessionDir, "setting.json"), JSON.stringify({front_compaction}));
      await assert.rejects(
        () => new SessionConfigLoader(sessionDir).loadSettings(),
        (error: unknown) => error instanceof ConfigError && /front_compaction/.test(error.message),
      );
    }
  } finally {
    await removeSession(sessionDir);
  }
});

test("resolves configurable session storage paths relative to setting.json", async () => {
  const sessionDir = await makeSession();
  const externalDir = await makeSession();
  const defaultModelPath = join(sessionDir, "model.json");
  const stagingPath = join(externalDir, "response-staging");
  const messagesPath = join(sessionDir, "data", "transcript.jsonl");
  const usageCachePath = join(sessionDir, "data", "usage-summary.json");
  const httpDebugPath = join(sessionDir, "diagnostics", "http");
  try {
    await writeFile(defaultModelPath, JSON.stringify({model: "configured-model"}));
    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({
        cwd: sessionDir,
        paths: {
          messages: "data/transcript.jsonl",
          messages_backup: "data/transcript.backup.jsonl",
          compaction_cache: "data/compact.json",
          history_cache: "data/input-history.json",
          usage_cache: "data/usage-summary.json",
          staging: stagingPath,
          http_debug: "diagnostics/http",
        },
      }),
    );

    const loader = new SessionConfigLoader(sessionDir);
    const settings = await loader.loadSettings();
    assert.equal(settings.paths.messages, messagesPath);
    assert.equal(settings.paths.messages_backup, join(sessionDir, "data/transcript.backup.jsonl"));
    assert.equal(settings.paths.compaction_cache, join(sessionDir, "data/compact.json"));
    assert.equal(settings.paths.history_cache, join(sessionDir, "data/input-history.json"));
    assert.equal(settings.paths.usage_cache, usageCachePath);
    assert.equal(settings.paths.staging, stagingPath);
    assert.equal(settings.paths.http_debug, httpDebugPath);

    const config = await loader.load();
    assert.equal(config.model.model, "configured-model");
    assert.equal(config.paths.messages_path, messagesPath);
    assert.equal(config.paths.messages_backup_path, join(sessionDir, "data/transcript.backup.jsonl"));
    assert.equal(config.paths.compaction_cache_path, join(sessionDir, "data/compact.json"));
    assert.equal(config.paths.history_cache_path, join(sessionDir, "data/input-history.json"));
    assert.equal(config.paths.usage_cache_path, usageCachePath);
    assert.equal(config.paths.model_path, defaultModelPath);
    assert.equal(config.paths.staging_dir, stagingPath);
    assert.equal(config.paths.http_debug_dir, httpDebugPath);

    const session = await AgentSession.open(sessionDir);
    assert.deepEqual(await readdir(stagingPath), []);
    await session.close();
    await assert.rejects(() => readdir(join(sessionDir, "staging")));

    const store = await SessionResponseStore.open(sessionDir, {
      messagesPath,
      stagingDir: stagingPath,
      usageCachePath,
    });
    assert.equal(store.usageCachePath, usageCachePath);
    await store.appendInputItem({
      type: "message",
      role: "user",
      content: [{type: "input_text", text: "configured storage"}],
    });
    assert.match(await readFile(messagesPath, "utf8"), /configured storage/);
    await store.close();
  } finally {
    await removeSession(sessionDir);
    await removeSession(externalDir);
  }
});

test("resolves a requested path profile without moving the session default", async () => {
  const sessionDir = await makeSession();
  await writeFile(join(sessionDir, "shared.md"), "Shared project guidance.");
  await writeFile(join(sessionDir, "blue.md"), "Blue profile guidance.");
  await writeFile(join(sessionDir, "green.md"), "Green profile guidance.");
  await mkdir(join(sessionDir, "chatgpt"), {recursive: true});
  await writeFile(join(sessionDir, "chatgpt/model.json"), JSON.stringify({model: "blue-model"}));
  await mkdir(join(sessionDir, "alternate"), {recursive: true});
  await writeFile(join(sessionDir, "alternate/model.json"), JSON.stringify({model: "green-model"}));
  await writeFile(join(sessionDir, "setting.json"), JSON.stringify({
    cwd: sessionDir,
    prompt_documents: ["shared.md"],
    paths: {
      active_profile: "blue",
      profiles: {
        blue: {
          messages: "profiles/blue/messages.jsonl",
          staging: "profiles/blue/staging",
          model_default: "chatgpt/model.json",
          prompt_documents: ["blue.md"],
        },
        green: {
          messages: "profiles/green/messages.jsonl",
          staging: "profiles/green/staging",
          model_default: "alternate/model.json",
          prompt_documents: ["green.md"],
        },
      },
    },
  }));

  try {
    const blue = new SessionConfigLoader(sessionDir, {profile: "blue"});
    const green = new SessionConfigLoader(sessionDir, {profile: "green"});
    const blueSettings = await blue.loadSettings();
    const greenSettings = await green.loadSettings();

    assert.equal(blueSettings.paths.active_profile, "blue");
    assert.equal(greenSettings.paths.active_profile, "green");
    // Both loaders report the same stored default: only the resolved profile differs.
    assert.equal(blueSettings.paths.default_profile, "blue");
    assert.equal(greenSettings.paths.default_profile, "blue");
    assert.deepEqual(blueSettings.prompt_documents, [
      join(sessionDir, "shared.md"),
      join(sessionDir, "blue.md"),
    ]);
    assert.equal(blueSettings.prompt_text, "Shared project guidance.\n\nBlue profile guidance.");
    assert.deepEqual(greenSettings.prompt_documents, [
      join(sessionDir, "shared.md"),
      join(sessionDir, "green.md"),
    ]);
    assert.equal(greenSettings.prompt_text, "Shared project guidance.\n\nGreen profile guidance.");
    assert.equal(blueSettings.paths.messages, join(sessionDir, "profiles/blue/messages.jsonl"));
    assert.equal(greenSettings.paths.messages, join(sessionDir, "profiles/green/messages.jsonl"));
    assert.notEqual(blueSettings.paths.staging, greenSettings.paths.staging);
    assert.equal((await blue.load()).model.model, "blue-model");
    assert.equal((await green.load()).model.model, "green-model");

    // An unqualified loader still follows the stored default, and a switch of it
    // does not disturb a loader pinned to a profile — that is what lets two
    // profiles of one session stay open at the same time.
    const stored = new SessionConfigLoader(sessionDir);
    assert.equal((await stored.loadSettings()).paths.active_profile, "blue");
    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({
      cwd: sessionDir,
      paths: {
        active_profile: "green",
        profiles: {
          blue: {
            messages: "profiles/blue/messages.jsonl",
            staging: "profiles/blue/staging",
            model_default: "chatgpt/model.json",
          },
          green: {
            messages: "profiles/green/messages.jsonl",
            staging: "profiles/green/staging",
            model_default: "alternate/model.json",
          },
        },
      },
    }));
    const blueAfterSwitch = await blue.loadSettings();
    assert.equal(blueAfterSwitch.paths.active_profile, "blue");
    assert.equal(blueAfterSwitch.paths.messages, join(sessionDir, "profiles/blue/messages.jsonl"));
    assert.equal((await green.loadSettings()).paths.default_profile, "green");
    // The unqualified loader pinned the old default, exactly as before profiles
    // were addressable: it refuses rather than silently reading another profile.
    await assert.rejects(() => stored.loadSettings(), /paths cannot change while this config loader is active/);

    // A requested profile must exist, and a session without profiles has none.
    await assert.rejects(
      () => new SessionConfigLoader(sessionDir, {profile: "missing"}).loadSettings(),
      /path profile 'missing' is not defined/,
    );
    await assert.rejects(
      () => new SessionConfigLoader(sessionDir, {profile: "bad name"}).loadSettings(),
      /must contain only letters, numbers, underscores, or hyphens/,
    );
    const plainDir = await makeSession();
    try {
      await assert.rejects(
        () => new SessionConfigLoader(plainDir, {profile: "blue"}).loadSettings(),
        /configures no paths\.profiles/,
      );
    } finally {
      await removeSession(plainDir);
    }
  } finally {
    await removeSession(sessionDir);
  }
});

test("validates prompt documents configured on path profiles", async () => {
  const sessionDir = await makeSession();
  const settingPath = join(sessionDir, "setting.json");
  try {
    await writeFile(settingPath, JSON.stringify({
      cwd: sessionDir,
      paths: {
        active_profile: "primary",
        profiles: {primary: {prompt_documents: "instructions.md"}},
      },
    }));
    await assert.rejects(
      () => new SessionConfigLoader(sessionDir).loadSettings(),
      /paths\.profiles\.primary: prompt_documents must be an array/,
    );

    await writeFile(settingPath, JSON.stringify({
      cwd: sessionDir,
      paths: {
        active_profile: "primary",
        profiles: {primary: {prompt_documents: [42]}},
      },
    }));
    await assert.rejects(
      () => new SessionConfigLoader(sessionDir).loadSettings(),
      /prompt_documents\[0\] must be a non-empty string/,
    );

    await writeFile(settingPath, JSON.stringify({
      cwd: sessionDir,
      paths: {
        active_profile: "primary",
        profiles: {
          primary: {prompt_documents: []},
          alternate: {prompt_documents: ["missing.md"]},
        },
      },
    }));
    const primarySettings = await new SessionConfigLoader(sessionDir, {profile: "primary"}).loadSettings();
    assert.deepEqual(primarySettings.prompt_documents, []);
    await assert.rejects(
      () => new SessionConfigLoader(sessionDir, {profile: "alternate"}).loadSettings(),
      /paths\.profiles\.alternate: prompt_documents\[0\] could not be read/,
    );
  } finally {
    await removeSession(sessionDir);
  }
});

test("profile storage defaults are isolated while the stored default keeps legacy paths", async () => {
  const sessionDir = await makeSession();
  try {
    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({
      cwd: sessionDir,
      paths: {
        active_profile: "primary",
        profiles: {primary: {}, alternate: {}},
      },
    }));

    const primary = new SessionConfigLoader(sessionDir, {profile: "primary"});
    const alternate = new SessionConfigLoader(sessionDir, {profile: "alternate"});
    const primaryPaths = (await primary.loadSettings()).paths;
    const alternatePaths = (await alternate.loadSettings()).paths;

    assert.equal(primaryPaths.messages, join(sessionDir, "messages.jsonl"));
    assert.equal(primaryPaths.staging, join(sessionDir, "staging"));
    assert.equal(alternatePaths.messages, join(sessionDir, "profiles/alternate/messages.jsonl"));
    assert.equal(alternatePaths.staging, join(sessionDir, "profiles/alternate/staging"));
    assert.notEqual(primaryPaths.messages, alternatePaths.messages);
    assert.notEqual(primaryPaths.compaction_cache, alternatePaths.compaction_cache);
    assert.notEqual(primaryPaths.history_cache, alternatePaths.history_cache);
    assert.notEqual(primaryPaths.usage_cache, alternatePaths.usage_cache);
    assert.notEqual(primaryPaths.cursor_cache, alternatePaths.cursor_cache);
    assert.notEqual(primaryPaths.run_error_cache, alternatePaths.run_error_cache);
    assert.equal(primaryPaths.run_error_cache, join(sessionDir, "staging", "run-error-cache.json"));
    assert.equal(alternatePaths.run_error_cache, join(sessionDir, "profiles/alternate/staging", "run-error-cache.json"));
  } finally {
    await removeSession(sessionDir);
  }
});

test("resolves native tool storage under the selected profile root", async () => {
  const sessionDir = await makeSession();
  const settingPath = join(sessionDir, "setting.json");
  const cwd = join(sessionDir, "workspace");
  const sharedMemoryStorage = join(sessionDir, "shared-memory");
  const absoluteToolRoot = join(sessionDir, "absolute-tools");
  try {
    await mkdir(cwd);
    await writeFile(settingPath, JSON.stringify({
      cwd,
      paths: {
        active_profile: "primary",
        profiles: {
          primary: {},
          alternate: {tool_storage: "custom/alternate-tools"},
          absolute: {tool_storage: absoluteToolRoot},
        },
      },
      tools: {
        native: {
          bash: {spill: {storage_dir: "bash-results", allow_unreadable: true}},
          task: {storage_dir: "task-results"},
          memory: {storage_dir: sharedMemoryStorage},
          fetch: {},
          process: {},
        },
      },
    }));

    const primary = new SessionConfigLoader(sessionDir, {profile: "primary"});
    const alternate = new SessionConfigLoader(sessionDir, {profile: "alternate"});
    const absolute = new SessionConfigLoader(sessionDir, {profile: "absolute"});
    const primarySettings = await primary.loadSettings();
    const alternateSettings = await alternate.loadSettings();
    const absoluteSettings = await absolute.loadSettings();
    const primaryRoot = join(cwd, ".jad", "profiles", "primary", "tools");
    const alternateRoot = join(cwd, "custom", "alternate-tools");

    assert.equal(primarySettings.paths.tool_storage, primaryRoot);
    assert.equal(alternateSettings.paths.tool_storage, alternateRoot);
    assert.equal(absoluteSettings.paths.tool_storage, absoluteToolRoot);
    for (const settings of [primarySettings, alternateSettings, absoluteSettings]) {
      const toolRoot = settings.paths.tool_storage;
      assert.ok(toolRoot, "profile tool_storage root should be set");
      assert.equal(settings.tools.native.bash?.spill.storage_dir, join(toolRoot, "bash-results"));
      assert.equal(settings.tools.native.task?.storage_dir, join(toolRoot, "task-results"));
      assert.equal(settings.tools.native.memory?.storage_dir, sharedMemoryStorage);
      assert.equal(settings.tools.native.fetch?.storage_dir, join(toolRoot, ".fetch"));
      assert.equal(settings.tools.native.process?.storage_dir, join(toolRoot, ".processes"));
    }

    await writeFile(settingPath, JSON.stringify({
      cwd,
      paths: {
        active_profile: "primary",
        profiles: {
          primary: {tool_storage: "moved-tools"},
          alternate: {tool_storage: "custom/alternate-tools"},
          absolute: {tool_storage: absoluteToolRoot},
        },
      },
    }));
    await assert.rejects(
      () => primary.loadSettings(),
      /session storage paths cannot change while this config loader is active/,
    );

    await writeFile(settingPath, JSON.stringify({
      cwd,
      tools: {native: {task: {storage_dir: "task-results"}}},
    }));
    const unprofiledSettings = await new SessionConfigLoader(sessionDir).loadSettings();
    assert.equal(unprofiledSettings.paths.tool_storage, undefined);
    assert.equal(unprofiledSettings.tools.native.task?.storage_dir, join(cwd, "task-results"));

    await writeFile(settingPath, JSON.stringify({cwd, paths: {tool_storage: "top-level-tools"}}));
    await assert.rejects(
      () => new SessionConfigLoader(sessionDir).loadSettings(),
      /paths\.tool_storage must be configured within paths\.profiles/,
    );
  } finally {
    await removeSession(sessionDir);
  }
});

test("scopes default and relative mutation backups to profile tool_storage", async () => {
  const sessionDir = await makeSession();
  const cwd = join(sessionDir, "workspace");
  try {
    await mkdir(cwd);
    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({
      cwd,
      paths: {
        active_profile: "reviewer",
        profiles: {reviewer: {}},
      },
      tools: {native: {write: {}, edit: {backup_dir: "edit-backups"}}},
    }));
    const settings = await new SessionConfigLoader(sessionDir).loadSettings();
    const toolRoot = join(cwd, ".jad", "profiles", "reviewer", "tools");
    assert.equal(settings.tools.native.write?.backup_dir, join(toolRoot, "backups"));
    assert.equal(settings.tools.native.edit?.backup_dir, join(toolRoot, "edit-backups"));
  } finally {
    await removeSession(sessionDir);
  }
});

test("rejects path profiles that explicitly share transcript or cache storage", async () => {
  const sessionDir = await makeSession();
  try {
    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({
      paths: {
        active_profile: "primary",
        profiles: {
          primary: {messages: "shared/messages.jsonl", staging: "shared/staging"},
          alternate: {messages: "shared/messages.jsonl", staging: "shared/staging"},
        },
      },
    }));

    await assert.rejects(
      () => new SessionConfigLoader(sessionDir).loadSettings(),
      /same storage path/,
    );
  } finally {
    await removeSession(sessionDir);
  }
});

test("selects named path profiles, merges overrides, and pins the choice to the loader", async () => {
  const sessionDir = await makeSession();
  const profileModelPath = join(sessionDir, "chatgpt/model.json");
  const alternateModelPath = join(sessionDir, "alternate/model.json");
  const profileStagingPath = join(sessionDir, "profiles/blue/staging");
  const profileUsageCachePath = join(sessionDir, "profiles/blue/custom-usage-cache.json");
  const profileCursorCachePath = join(sessionDir, "profiles/blue/custom-cursor-cache.json");
  try {
    await mkdir(dirname(profileModelPath), {recursive: true});
    await mkdir(dirname(alternateModelPath), {recursive: true});
    await writeFile(profileModelPath, JSON.stringify({model: "profile-blue-model"}));
    await writeFile(alternateModelPath, JSON.stringify({model: "profile-alternate-model"}));
    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({
      cwd: sessionDir,
      paths: {
        messages: "base/messages.jsonl",
        messages_backup: "base/messages.backup.jsonl",
        compaction_cache: "base/staging/compaction-cache.json",
        statecontext: "base/staging/statecontext.json",
        staging: "base/staging",
        http_debug: "base/debug/http",
        active_profile: "blue",
        profiles: {
          blue: {
            messages: "profiles/blue/messages.jsonl",
            messages_backup: "profiles/blue/messages.backup.jsonl",
            compaction_cache: "profiles/blue/staging/compaction-cache.json",
            statecontext: "profiles/blue/staging/statecontext.json",
            usage_cache: profileUsageCachePath,
            cursor_cache: profileCursorCachePath,
            model_default: "chatgpt/model.json",
            models: ["chatgpt/model.json", "alternate/model.json"],
            staging: "profiles/blue/staging",
            http_debug: "profiles/blue/debug/http",
          },
          green: {},
        },
      },
    }));

    const loader = new SessionConfigLoader(sessionDir);
    const settings = await loader.loadSettings();
    assert.equal(settings.paths.active_profile, "blue");
    assert.equal(settings.paths.model_default, "chatgpt/model.json");
    assert.deepEqual(settings.paths.models, ["chatgpt/model.json", "alternate/model.json"]);
    assert.equal(settings.paths.messages, join(sessionDir, "profiles/blue/messages.jsonl"));
    assert.equal(settings.paths.history_cache, join(profileStagingPath, "history-cache.json"));
    assert.equal(settings.paths.usage_cache, profileUsageCachePath);
    assert.equal(settings.paths.cursor_cache, profileCursorCachePath);
    assert.equal(settings.paths.http_debug, join(sessionDir, "profiles/blue/debug/http"));
    assert.equal(loader.paths.model_path, profileModelPath);
    assert.equal((await loader.load()).model.model, "profile-blue-model");
    assert.equal((await loader.load("alternate/model.json")).model.model, "profile-alternate-model");
    assert.equal(loader.paths.statecontext_path, join(profileStagingPath, "statecontext.json"));
    assert.equal(loader.paths.usage_cache_path, profileUsageCachePath);
    assert.equal(loader.paths.cursor_cache_path, profileCursorCachePath);
    assert.equal(loader.paths.active_profile, "blue");

    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({
      cwd: sessionDir,
      paths: {
        active_profile: "green",
        profiles: {
          green: {},
        },
      },
    }));
    await assert.rejects(() => loader.loadSettings(), /paths cannot change while this config loader is active/);

    // A profile that does not name the cursor cache still gets one in its own
    // staging directory, so profiles keep separate read indexes.
    const unselected = new SessionConfigLoader(sessionDir);
    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({
      cwd: sessionDir,
      paths: {
        active_profile: "green",
        profiles: {
          green: {
            messages: "profiles/green/messages.jsonl",
            staging: "profiles/green/staging",
          },
        },
      },
    }));
    const greenPaths = (await unselected.loadSettings()).paths;
    assert.equal(greenPaths.messages_backup, join(sessionDir, "profiles/green/messages.backup.jsonl"));
    assert.equal(greenPaths.compaction_cache, join(sessionDir, "profiles/green/staging", "compaction-cache.json"));
    assert.equal(greenPaths.statecontext, join(sessionDir, "profiles/green/staging", "statecontext.json"));
    assert.equal(greenPaths.history_cache, join(sessionDir, "profiles/green/staging", "history-cache.json"));
    assert.equal(greenPaths.usage_cache, join(sessionDir, "profiles/green/staging", "usage-cache.json"));
    assert.equal(greenPaths.cursor_cache, join(sessionDir, "profiles/green/staging", "cursor-cache.json"));
    assert.equal(unselected.paths.model_path, join(sessionDir, "model.json"));
  } finally {
    await removeSession(sessionDir);
  }
});

test("refreshes model-config paths while pinning storage paths to the loader", async () => {
  const sessionDir = await makeSession();
  const settingPath = join(sessionDir, "setting.json");
  try {
    const loader = new SessionConfigLoader(sessionDir);
    await writeFile(settingPath, JSON.stringify({
      cwd: sessionDir,
      paths: {
        active_profile: "blue",
        profiles: {blue: {models: ["chatgpt/model.json", "openai/model.json"]}},
      },
    }));
    const settings = await loader.loadSettings();
    assert.equal(settings.paths.model_default, "chatgpt/model.json");
    assert.deepEqual(settings.paths.models, ["chatgpt/model.json", "openai/model.json"]);

    await writeFile(settingPath, JSON.stringify({
      cwd: sessionDir,
      paths: {
        active_profile: "blue",
        profiles: {blue: {model_default: "openai/model.json", models: ["openai/model.json", "chatgpt/model.json"]}},
      },
    }));
    const updatedModelSettings = await loader.loadSettings();
    assert.equal(updatedModelSettings.paths.model_default, "openai/model.json");
    assert.deepEqual(updatedModelSettings.paths.models, ["openai/model.json", "chatgpt/model.json"]);
    assert.equal(loader.paths.model_path, join(sessionDir, "openai/model.json"));

    await writeFile(settingPath, JSON.stringify({
      cwd: sessionDir,
      paths: {
        active_profile: "blue",
        profiles: {
          blue: {
            model_default: "openai/model.json",
            models: ["chatgpt/model.json", "openai/model.json"],
            messages: "changed/messages.jsonl",
          },
        },
      },
    }));
    await assert.rejects(() => loader.loadSettings(), /session storage paths cannot change while this config loader is active/);

    await writeFile(settingPath, JSON.stringify({
      cwd: sessionDir,
      paths: {
        active_profile: "single",
        profiles: {single: {model_default: "solo/model.json"}},
      },
    }));
    const singleChoice = await new SessionConfigLoader(sessionDir).loadSettings();
    assert.equal(singleChoice.paths.model_default, "solo/model.json");
    assert.deepEqual(singleChoice.paths.models, ["solo/model.json"]);
  } finally {
    await removeSession(sessionDir);
  }
});

test("rejects malformed and conflicting path profiles", async () => {
  const sessionDir = await makeSession();
  try {
    const loader = new SessionConfigLoader(sessionDir);
    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({paths: {profiles: {profile1: {}}}}));
    await assert.rejects(() => loader.loadSettings(), /active_profile/);

    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({paths: {active_profile: "missing", profiles: {profile1: {}}}}));
    await assert.rejects(() => loader.loadSettings(), /not defined in paths\.profiles/);

    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({
      paths: {active_profile: "profile1", profiles: {profile1: {messages: "same.jsonl", messages_backup: "same.jsonl"}}},
    }));
    await assert.rejects(() => loader.loadSettings(), /must be different files/);

    // The cursor index is a session file like any other: it may not alias one.
    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({
      paths: {
        active_profile: "profile1",
        profiles: {profile1: {usage_cache: "staging/shared.json", cursor_cache: "staging/shared.json"}},
      },
    }));
    await assert.rejects(() => loader.loadSettings(), /must be different files/);

    // The failed-run cache is a session file too: it may not alias the cursor index.
    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({
      paths: {
        active_profile: "profile1",
        profiles: {profile1: {cursor_cache: "staging/shared.json", run_error_cache: "staging/shared.json"}},
      },
    }));
    await assert.rejects(() => loader.loadSettings(), /paths\.run_error_cache.*must be different files/);

    // An unknown profile field is still rejected, so a typo cannot silently
    // fall back to the flat path.
    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({
      paths: {active_profile: "profile1", profiles: {profile1: {cursor_caches: "staging/cursor-cache.json"}}},
    }));
    await assert.rejects(() => loader.loadSettings(), /is not a supported session path/);

    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({
      paths: {active_profile: "profile1", profiles: {profile1: {models: "model-a"}}},
    }));
    await assert.rejects(() => loader.loadSettings(), /models must be an array/);

    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({
      paths: {
        active_profile: "profile1",
        profiles: {profile1: {model_default: "chatgpt/model.json", models: ["other/model.json"]}},
      },
    }));
    await assert.rejects(() => loader.loadSettings(), /model_default must be included in models/);

    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({
      paths: {active_profile: "profile1", profiles: {profile1: {models: ["chatgpt/model.json", "chatgpt/model.json"]}}},
    }));
    await assert.rejects(() => loader.loadSettings(), /duplicate paths/);

    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({
      paths: {model_default: "model-a", models: ["model-a"]},
    }));
    await assert.rejects(() => loader.loadSettings(), /must be configured within paths\.profiles/);

    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({
      paths: {model: "chatgpt/model.json"},
    }));
    await assert.rejects(() => loader.loadSettings(), /paths\.model was removed/);

    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({
      paths: {active_profile: "profile1", profiles: {profile1: {model: "chatgpt/model.json"}}},
    }));
    await assert.rejects(() => loader.loadSettings(), /profile1\.model was removed/);
  } finally {
    await removeSession(sessionDir);
  }
});

test("compacts to the final assistant response, backs up transcript, and rebuilds request history", async () => {
  const sessionDir = await makeSession();
  try {
    const store = await SessionResponseStore.open(sessionDir, {durability: "strict"});
    const user = {type: "message", role: "user", content: [{type: "input_text", text: "question"}]};
    const reasoning = {type: "reasoning", summary: [{type: "summary_text", text: "private reasoning"}]};
    const call = {type: "function_call", call_id: "call-large", name: "search", arguments: "{}"};
    const partial = {type: "message", role: "assistant", content: [{type: "output_text", text: "Searching"}]};
    const output = {type: "function_call_output", call_id: "call-large", output: "x".repeat(10000)};
    const final = {type: "message", role: "assistant", content: [{type: "output_text", text: "Answer"}]};
    await store.appendInputItem(user);
    const toolTurn = await store.beginTurn("compact-tool-call");
    for (const [output_index, item] of [reasoning, partial, call].entries()) {
      await toolTurn.stageOutputItem({type: "response.output_item.done", output_index, item});
    }
    await toolTurn.commit(usageEvent([reasoning, partial, call], "resp-tool-call", {
      input_tokens: 100,
      output_tokens: 20,
      input_tokens_details: {cached_tokens: 7},
    }, 1, 1));
    await store.appendInputItem(output);
    const finalTurn = await store.beginTurn("compact-final");
    await finalTurn.stageOutputItem({type: "response.output_item.done", output_index: 0, item: final});
    await finalTurn.commit(usageEvent([final], "resp-final", {
      input_tokens: 30,
      output_tokens: 5,
    }, 1, 2));

    const beforeUsage = await store.readUsageSummary();
    const before = await readFile(store.messagesPath, "utf8");
    const result = await store.compactHistory({turnType: "assistant_final", keepTurns: 1, keepToolPairs: 0, backupMode: "replace"});
    assert.equal(result.compacted, true);
    assert.equal((await readFile(store.messagesPath, "utf8")), "");
    const afterCompaction = await store.readUsageSummary();
    assert.equal(afterCompaction.completed_responses, beforeUsage.completed_responses);
    assert.equal(afterCompaction.responses_with_usage, beforeUsage.responses_with_usage);
    assert.equal(afterCompaction.turn_count, beforeUsage.turn_count);
    assert.equal(afterCompaction.input_tokens, beforeUsage.input_tokens);
    assert.equal(afterCompaction.output_tokens, beforeUsage.output_tokens);
    assert.equal(afterCompaction.total_tokens, beforeUsage.total_tokens);
    assert.equal(afterCompaction.cached_input_tokens, beforeUsage.cached_input_tokens);
    assert.equal(afterCompaction.current?.input_tokens, 0);
    assert.equal(afterCompaction.current?.output_tokens, 0);
    assert.equal(afterCompaction.current?.total_tokens, 0);
    assert.equal(afterCompaction.current?.cached_input_tokens, 0);
    assert.equal(afterCompaction.current?.turn, beforeUsage.current?.turn);
    assert.equal(afterCompaction.current?.step, beforeUsage.current?.step);
    assert.equal(await readFile(store.messagesBackupPath, "utf8"), before);
    const history = [];
    for await (const item of store.readInputItems()) history.push(item);
    assert.deepEqual(history, [user, final]);
    assert.equal((await readFile(store.compactionCacheBackupPath, "utf8")).length > 0, true);
    const compactionCache = await readFile(store.compactionCachePath, "utf8");
    const noOpCompaction = await store.compactHistory({
      turnType: "assistant_final", keepTurns: 1, keepToolPairs: 0, backupMode: "replace",
    });
    assert.equal(noOpCompaction.compacted, false);
    assert.deepEqual(await store.readUsageSummary(), afterCompaction);
    assert.equal(await readFile(store.compactionCachePath, "utf8"), compactionCache);
    await rm(store.compactionCachePath);
    const recoveredHistory = [];
    for await (const item of store.readInputItems()) recoveredHistory.push(item);
    assert.deepEqual(recoveredHistory, [user, final]);
    await store.appendInputItem({type: "message", role: "user", content: [{type: "input_text", text: "follow-up"}]});
    const combined = [];
    for await (const item of store.readInputItems()) combined.push(item);
    const followUp = {type: "message", role: "user", content: [{type: "input_text", text: "follow-up"}]};
    assert.deepEqual(combined, [user, final, followUp]);
    const afterFollowUp = await store.readUsageSummary();
    assert.equal(afterFollowUp.turn_count, 2);
    assert.equal(afterFollowUp.completed_responses, 2);
    const followUpTranscript = await readFile(store.messagesPath, "utf8");
    const appended = await store.compactHistory({turnType: "assistant_final", keepTurns: 1, keepToolPairs: 0, backupMode: "append"});
    assert.equal(appended.compacted, true);
    assert.equal(await readFile(store.messagesBackupPath, "utf8"), `${before}${followUpTranscript}`);
    assert.deepEqual(await store.readUsageSummary(), afterFollowUp);
    await assert.rejects(() => readFile(store.historyCachePath, "utf8"), {code: "ENOENT"});
    await store.close();

    await rm(store.usageCachePath);
    const rebuilt = await SessionResponseStore.open(sessionDir);
    const rebuiltUsage = await rebuilt.readUsageSummary();
    assert.equal(rebuiltUsage.completed_responses, 2);
    assert.equal(rebuiltUsage.responses_with_usage, 2);
    assert.equal(rebuiltUsage.turn_count, 2);
    assert.equal(rebuiltUsage.input_tokens, 130);
    assert.equal(rebuiltUsage.output_tokens, 25);
    assert.equal(rebuiltUsage.total_tokens, 155);
    assert.equal(rebuiltUsage.cached_input_tokens, 7);
    assert.equal(rebuiltUsage.current?.input_tokens, 0);
    assert.equal(rebuiltUsage.current?.output_tokens, 0);
    assert.equal(rebuiltUsage.current?.total_tokens, 0);
    assert.equal(rebuiltUsage.current?.cached_input_tokens, 0);
    assert.equal(rebuiltUsage.current?.turn, 1);
    assert.equal(rebuiltUsage.current?.step, 2);
    assert.equal(rebuiltUsage.history_partial, true);

    const postCompactionTurn = await rebuilt.beginTurn("post-compaction-response");
    await postCompactionTurn.stageOutputItem({type: "response.output_item.done", output_index: 0, item: final});
    await postCompactionTurn.commit(usageEvent([final], "resp-post-compaction", {
      input_tokens: 8,
      output_tokens: 3,
    }, 3, 1));
    const afterPostCompactionResponse = await rebuilt.readUsageSummary();
    assert.equal(afterPostCompactionResponse.current?.input_tokens, 8);
    assert.equal(afterPostCompactionResponse.current?.output_tokens, 3);
    assert.equal(afterPostCompactionResponse.current?.total_tokens, 11);
    assert.equal(afterPostCompactionResponse.input_tokens, 138);
    assert.equal(afterPostCompactionResponse.output_tokens, 28);
    assert.equal(afterPostCompactionResponse.total_tokens, 166);
    await rebuilt.close();

    await rm(store.usageCachePath);
    const rebuiltAgain = await SessionResponseStore.open(sessionDir);
    const rebuiltAgainUsage = await rebuiltAgain.readUsageSummary();
    assert.equal(rebuiltAgainUsage.current?.input_tokens, 8);
    assert.equal(rebuiltAgainUsage.current?.output_tokens, 3);
    assert.equal(rebuiltAgainUsage.current?.total_tokens, 11);
    assert.equal(rebuiltAgainUsage.input_tokens, 138);
    assert.equal(rebuiltAgainUsage.output_tokens, 28);
    assert.equal(rebuiltAgainUsage.total_tokens, 166);
    await rebuiltAgain.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("compacts at a tool-call boundary while leaving that response active for its result", async () => {
  const sessionDir = await makeSession();
  try {
    const store = await SessionResponseStore.open(sessionDir);
    const oldUser = {type: "message", role: "user", content: [{type: "input_text", text: "old turn"}]};
    const oldCall = {type: "function_call", call_id: "old-call", name: "lookup", arguments: "{}"};
    const oldOutput = {type: "function_call_output", call_id: "old-call", output: "large tool data ".repeat(500)};
    const oldFinal = {type: "message", role: "assistant", content: [{type: "output_text", text: "old answer"}]};
    const currentUser = {type: "message", role: "user", content: [{type: "input_text", text: "new turn"}]};
    const currentReasoning = {
      type: "reasoning",
      summary: [{type: "summary_text", text: "private reasoning"}],
      encrypted_content: "encrypted-reasoning-payload".repeat(100),
    };
    const currentPartial = {
      type: "message",
      role: "assistant",
      content: [{type: "output_text", text: "partial assistant response ".repeat(100)}],
    };
    const currentCall = {type: "function_call", call_id: "current-call", name: "lookup", arguments: "{}"};
    await store.appendInputItem(oldUser);
    const oldToolTurn = await store.beginTurn("old-tool-response");
    await oldToolTurn.stageOutputItem({type: "response.output_item.done", output_index: 0, item: oldCall});
    await oldToolTurn.commit(completedEvent([oldCall], "resp-old-call"));
    await store.appendInputItem(oldOutput);
    const oldFinalTurn = await store.beginTurn("old-final-response");
    await oldFinalTurn.stageOutputItem({type: "response.output_item.done", output_index: 0, item: oldFinal});
    await oldFinalTurn.commit(completedEvent([oldFinal], "resp-old-final"));
    await store.appendInputItem(currentUser);
    const currentToolTurn = await store.beginTurn("current-tool-response");
    const currentOutput = [currentReasoning, currentPartial, currentCall];
    for (const [output_index, item] of currentOutput.entries()) {
      await currentToolTurn.stageOutputItem({type: "response.output_item.done", output_index, item});
    }
    const currentResponse = completedEvent(currentOutput, "resp-current-call");
    Object.assign(currentResponse.response, {output_text: "partial response convenience field"});
    await currentToolTurn.commit(currentResponse);
    const originalTranscript = await readFile(store.messagesPath, "utf8");

    const compacted = await store.compactHistory({
      turnType: "assistant_final", keepTurns: 2, keepToolPairs: 1, backupMode: "replace",
      preserveResponseId: "resp-current-call",
    });
    assert.equal(compacted.compacted, true);
    const history = [];
    for await (const item of store.readInputItems()) history.push(item);
    assert.deepEqual(history, [oldUser, oldCall, oldOutput, oldFinal, currentUser, currentCall]);

    const activeTranscript = await readFile(store.messagesPath, "utf8");
    assert.doesNotMatch(activeTranscript, /old-call|large tool data|encrypted-reasoning-payload|partial assistant response/);
    assert.match(activeTranscript, /resp-current-call/);
    const activeResponse = JSON.parse(activeTranscript.trim()) as {
      response: {output: unknown[]; output_text?: unknown};
    };
    assert.deepEqual(activeResponse.response.output, [currentCall]);
    assert.equal(Object.hasOwn(activeResponse.response, "output_text"), false);

    const backupTranscript = await readFile(store.messagesBackupPath, "utf8");
    assert.equal(backupTranscript, originalTranscript);
    assert.match(backupTranscript, /encrypted-reasoning-payload/);
    const compactionCache = JSON.parse(await readFile(store.compactionCachePath, "utf8")) as {
      items: unknown[];
      active_tail: Array<{response: {output: unknown[]; output_text?: unknown}}>;
    };
    assert.deepEqual(compactionCache.items, [oldUser, oldCall, oldOutput, oldFinal, currentUser]);
    assert.deepEqual(compactionCache.active_tail[0]?.response.output, [currentCall]);
    assert.equal(Object.hasOwn(compactionCache.active_tail[0]!.response, "output_text"), false);
    assert.doesNotMatch(JSON.stringify(compactionCache), /encrypted-reasoning-payload|partial assistant response/);

    await store.appendInputItem({type: "function_call_output", call_id: "current-call", output: "result"});
    const nextRequestHistory = [];
    for await (const item of store.readInputItems()) nextRequestHistory.push(item);
    assert.deepEqual(nextRequestHistory, [
      oldUser,
      oldCall,
      oldOutput,
      oldFinal,
      currentUser,
      currentCall,
      {type: "function_call_output", call_id: "current-call", output: "result"},
    ]);
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("recovers an interrupted compaction before new transcript records are appended", async () => {
  const sessionDir = await makeSession();
  try {
    const store = await SessionResponseStore.open(sessionDir);
    const user = {type: "message", role: "user", content: [{type: "input_text", text: "before crash"}]};
    const reasoning = {type: "reasoning", summary: [{type: "summary_text", text: "long reasoning ".repeat(100)}]};
    const assistant = {type: "message", role: "assistant", content: [{type: "output_text", text: "answer"}]};
    await store.appendInputItem(user);
    const turn = await store.beginTurn("crash-compaction");
    await turn.stageOutputItem({type: "response.output_item.done", output_index: 0, item: reasoning});
    await turn.stageOutputItem({type: "response.output_item.done", output_index: 1, item: assistant});
    await turn.commit(completedEvent([reasoning, assistant], "resp-crash-compaction"));
    const originalTranscript = await readFile(store.messagesPath, "utf8");
    assert.equal((await store.compactHistory({turnType: "assistant_final", keepTurns: 1, keepToolPairs: 0, backupMode: "replace"})).compacted, true);
    await store.close();

    await writeFile(store.messagesPath, originalTranscript, "utf8");
    const recovered = await SessionResponseStore.open(sessionDir);
    assert.equal(await readFile(recovered.messagesPath, "utf8"), "");
    await recovered.appendInputItem({type: "message", role: "user", content: [{type: "input_text", text: "after recovery"}]});
    const history = [];
    for await (const item of recovered.readInputItems()) history.push(item);
    assert.deepEqual(history, [user, assistant, {type: "message", role: "user", content: [{type: "input_text", text: "after recovery"}]}]);
    await recovered.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("refuses to silently send truncated history if both compaction cache copies are lost", async () => {
  const sessionDir = await makeSession();
  try {
    const store = await SessionResponseStore.open(sessionDir);
    const user = {type: "message", role: "user", content: [{type: "input_text", text: "preserve this history"}]};
    const reasoning = {type: "reasoning", summary: [{type: "summary_text", text: "discarded ".repeat(500)}]};
    const answer = {type: "message", role: "assistant", content: [{type: "output_text", text: "answer"}]};
    await store.appendInputItem(user);
    const turn = await store.beginTurn("cache-loss");
    await turn.stageOutputItem({type: "response.output_item.done", output_index: 0, item: reasoning});
    await turn.stageOutputItem({type: "response.output_item.done", output_index: 1, item: answer});
    await turn.commit(completedEvent([reasoning, answer], "resp-cache-loss"));
    assert.equal((await store.compactHistory({
      turnType: "assistant_final", keepTurns: 1, keepToolPairs: 0, backupMode: "replace",
    })).compacted, true);
    await rm(store.compactionCachePath);
    await rm(store.compactionCacheBackupPath);
    await assert.rejects(
      async () => { for await (const _item of store.readInputItems()) { /* consume */ } },
      /compacted request history is unavailable/,
    );
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("assistant_final never mistakes a tool-call response for the turn summary", async () => {
  const sessionDir = await makeSession();
  try {
    const store = await SessionResponseStore.open(sessionDir);
    const firstUser = {type: "message", role: "user", content: [{type: "input_text", text: "first"}]};
    const oldFinal = {type: "message", role: "assistant", content: [{type: "output_text", text: "first answer"}]};
    const secondUser = {type: "message", role: "user", content: [{type: "input_text", text: "second"}]};
    const intermediate = {type: "message", role: "assistant", content: [{type: "output_text", text: "calling tool"}]};
    const call = {type: "function_call", call_id: "pending-call", name: "lookup", arguments: "{}"};
    await store.appendInputItem(firstUser);
    const first = await store.beginTurn("prior-final");
    await first.stageOutputItem({type: "response.output_item.done", output_index: 0, item: oldFinal});
    await first.commit(completedEvent([oldFinal], "resp-prior-final"));
    await store.appendInputItem(secondUser);
    const toolTurn = await store.beginTurn("pending-tool-turn");
    for (const [output_index, item] of [intermediate, call].entries()) {
      await toolTurn.stageOutputItem({type: "response.output_item.done", output_index, item});
    }
    await toolTurn.commit(completedEvent([intermediate, call], "resp-pending-call"));
    const result = await store.compactHistory({turnType: "assistant_final", keepTurns: 2, keepToolPairs: 0, backupMode: "replace"});
    assert.equal(result.compacted, true);
    const history = [];
    for await (const item of store.readInputItems()) history.push(item);
    assert.deepEqual(history, [firstUser, oldFinal, secondUser]);
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("full compaction keeps user, assistant, and reasoning but drops tool traffic", async () => {
  const sessionDir = await makeSession();
  try {
    const store = await SessionResponseStore.open(sessionDir);
    const user = {type: "message", role: "user", content: [{type: "input_text", text: "question"}]};
    const reasoning = {type: "reasoning", summary: [{type: "summary_text", text: "reasoning"}]};
    const assistant = {type: "message", role: "assistant", content: [{type: "output_text", text: "answer"}]};
    const call = {type: "function_call", call_id: "call-full", name: "search", arguments: "{}"};
    await store.appendInputItem(user);
    const turn = await store.beginTurn("compact-full");
    for (const [output_index, item] of [reasoning, assistant, call].entries()) {
      await turn.stageOutputItem({type: "response.output_item.done", output_index, item});
    }
    await turn.commit(completedEvent([reasoning, assistant, call]));
    await store.appendInputItem({type: "function_call_output", call_id: "call-full", output: "large result"});
    const result = await store.compactHistory({turnType: "full", keepTurns: 1, keepToolPairs: 1, backupMode: "replace"});
    assert.equal(result.compacted, true);
    const history = [];
    for await (const item of store.readInputItems()) history.push(item);
    assert.deepEqual(history, [user, reasoning, assistant]);
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("defaults empty settings and rejects missing or unsafe model configuration", async () => {
  const sessionDir = await makeSession();
  try {
    await writeFile(join(sessionDir, "model.json"), JSON.stringify({ model: "gpt-test" }));
    await writeFile(join(sessionDir, "setting.json"), "");
    const loader = new SessionConfigLoader(sessionDir);
    const settings = await loader.loadSettings();
    assert.equal(settings.session_name, sessionDir.split("/").pop());
    assert.equal(settings.cwd, process.cwd());
    assert.deepEqual(settings.prompt_documents, []);
    assert.equal(settings.prompt_role, "system");
    assert.equal(settings.prompt_text, "");
    assert.deepEqual(settings.persistence, {
      durability: "turn",
      max_item_bytes: 10 * 1024 * 1024,
      response_metadata: "minimal",
      cursor_cache_checkpoint_bytes: 1024 * 1024,
      run_error_cache_max_entries: DEFAULT_RUN_ERROR_CACHE_MAX_ENTRIES,
    });
    assert.equal(settings.http_debug.enabled, false);
    assert.equal(settings.paths.history_cache, join(sessionDir, "staging/history-cache.json"));
    assert.equal(loader.paths.history_cache_path, join(sessionDir, "staging/history-cache.json"));
    assert.equal(settings.paths.usage_cache, join(sessionDir, "staging/usage-cache.json"));
    assert.equal(loader.paths.usage_cache_path, join(sessionDir, "staging/usage-cache.json"));
    assert.equal(settings.http_debug.include_response_headers, true);
    assert.equal(settings.http_debug.max_body_bytes, 65536);

    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({cwd: sessionDir, system_prompt: "legacy"}));
    await assert.rejects(() => loader.loadSettings(), /system_prompt was removed/);
    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({cwd: sessionDir, prompt_documents: "base.md"}));
    await assert.rejects(() => loader.loadSettings(), /prompt_documents must be an array/);

    await writeFile(
      join(sessionDir, "model.json"),
      JSON.stringify({ model: "gpt-test", request: { stream: true } }),
    );
    await assert.rejects(() => loader.loadModel(), (error: unknown) => error instanceof ConfigError);
  } finally {
    await removeSession(sessionDir);
  }

  const missingModelDir = await makeSession();
  try {
    await assert.rejects(
      () => new SessionConfigLoader(missingModelDir).loadModel(),
      (error: unknown) => error instanceof ConfigError,
    );
  } finally {
    await removeSession(missingModelDir);
  }
});

test("resolves configured paths from the global JAD root and rejects relative cwd", async () => {
  const globalJadRoot = await makeSession();
  const sessionDir = join(globalJadRoot, "sessions", "session1");
  try {
    await mkdir(sessionDir, {recursive: true});
    await mkdir(join(globalJadRoot, "tools"), {recursive: true});
    await writeFile(join(globalJadRoot, "tools", "read.json"), "{}");
    await writeFile(join(sessionDir, "model.json"), JSON.stringify({model: "gpt-test"}));
    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({
        cwd: sessionDir,
        tool_definition_paths: ["tools/read.json"],
        tools: {
          native: {
            read: {
              paths: [{action: "allow", path: "tools"}],
            },
          },
        },
      }),
    );

    const loader = new SessionConfigLoader(sessionDir, {globalJadRoot});
    const settings = await loader.loadSettings();
    assert.equal(settings.cwd, sessionDir);
    assert.deepEqual(settings.tool_definition_paths, [
      join(globalJadRoot, "tools", "read.json"),
    ]);
    assert.deepEqual(settings.tools.native.read?.paths, [
      {action: "allow", path: join(globalJadRoot, "tools")},
    ]);
    assert.equal(loader.paths.global_jad_root, globalJadRoot);

    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({cwd: "relative/workspace"}),
    );
    await assert.rejects(
      () => loader.loadSettings(),
      (error: unknown) =>
        error instanceof ConfigError && /cwd must be an absolute path/.test(error.message),
    );
  } finally {
    await removeSession(globalJadRoot);
  }
});

test("normalizes and validates HTTP debug settings", async () => {
  const sessionDir = await makeSession();
  try {
    await writeFile(join(sessionDir, "model.json"), JSON.stringify({ model: "gpt-test" }));
    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({
        cwd: sessionDir,
        http_debug: {
          enabled: true,
          include_request_headers: true,
          include_request_body: true,
          include_response_body: true,
          max_body_bytes: 1024,
        },
      }),
    );
    const settings = await new SessionConfigLoader(sessionDir).loadSettings();
    assert.equal(settings.http_debug.enabled, true);
    assert.equal(settings.http_debug.include_request_headers, true);
    assert.equal(settings.http_debug.include_request_body, true);
    assert.equal(settings.http_debug.include_response_headers, true);
    assert.equal(settings.http_debug.include_response_body, true);
    assert.equal(settings.http_debug.max_body_bytes, 1024);

    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({ http_debug: { max_body_bytes: 0 } }),
    );
    await assert.rejects(
      () => new SessionConfigLoader(sessionDir).loadSettings(),
      (error: unknown) => error instanceof ConfigError,
    );
  } finally {
    await removeSession(sessionDir);
  }
});

test("auto-compacts at a tool-call stream end and preserves the in-flight call", async () => {
  const sessionDir = await makeSession();
  try {
    await writeFile(join(sessionDir, "model.json"), JSON.stringify({model: "gpt-test"}));
    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({cwd: sessionDir, max_tool_rounds: 0, compaction: {trigger_tokens: 1}}));
    const oldUser = {type: "message", role: "user", content: [{type: "input_text", text: "old question"}]};
    const oldCall = {type: "function_call", call_id: "old-call", name: "lookup", arguments: "{}"};
    const oldOutput = {type: "function_call_output", call_id: "old-call", output: "very large tool output ".repeat(500)};
    const oldAssistant = {type: "message", role: "assistant", content: [{type: "output_text", text: "old answer"}]};
    await writeFile(join(sessionDir, "messages.jsonl"), [
      oldUser,
      {type: "response.completed", response: {id: "old-call-response", status: "completed", output: [oldCall]}},
      oldOutput,
      {type: "response.completed", response: {id: "old-final-response", status: "completed", output: [oldAssistant]}},
    ].map((record) => JSON.stringify(record)).join("\n") + "\n");
    const currentCall = {type: "function_call", call_id: "current-call", name: "not-configured", arguments: "{}"};
    const fakeClient = {
      responses: {create: async () => (async function* () {
        yield {type: "response.output_item.done", output_index: 0, item: currentCall};
        yield completedEvent([currentCall], "resp-auto-tool-call");
      })()},
    } as unknown as OpenAI;
    const session = await AgentSession.open(sessionDir, {client: fakeClient});
    await assert.rejects(() => session.run("new question"), /exceeded 0 tool rounds/);
    const activeTranscript = await readFile(join(sessionDir, "messages.jsonl"), "utf8");
    assert.match(activeTranscript, /resp-auto-tool-call/);
    assert.doesNotMatch(activeTranscript, /very large tool output|old-call/);
    const store = await SessionResponseStore.open(sessionDir);
    const history = [];
    for await (const item of store.readInputItems()) history.push(item);
    assert.equal(history[0]?.role, "user");
    assert.equal(history[1]?.type, "function_call");
    assert.equal(history[1]?.call_id, "current-call");
    assert.equal(history[2]?.type, "function_call_output");
    const usageAfterCompaction = await session.readUsageSummary();
    assert.equal(usageAfterCompaction.input_tokens, 10);
    assert.equal(usageAfterCompaction.output_tokens, 4);
    assert.equal(usageAfterCompaction.total_tokens, 14);
    assert.equal(usageAfterCompaction.current?.input_tokens, 0);
    assert.equal(usageAfterCompaction.current?.output_tokens, 0);
    assert.equal(usageAfterCompaction.current?.total_tokens, 0);
    await store.close();
    await session.close();
    await rm(store.usageCachePath, {force: true});
    const rebuiltStore = await SessionResponseStore.open(sessionDir);
    const rebuiltUsage = await rebuiltStore.readUsageSummary();
    assert.equal(rebuiltUsage.input_tokens, 10);
    assert.equal(rebuiltUsage.output_tokens, 4);
    assert.equal(rebuiltUsage.total_tokens, 14);
    assert.equal(rebuiltUsage.current?.input_tokens, 0);
    assert.equal(rebuiltUsage.current?.output_tokens, 0);
    assert.equal(rebuiltUsage.current?.total_tokens, 0);
    await rebuiltStore.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("auto-compacts after a completed response and reuses the compacted context", async () => {
  const sessionDir = await makeSession();
  try {
    await writeFile(join(sessionDir, "model.json"), JSON.stringify({model: "gpt-test"}));
    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({
      cwd: sessionDir,
      compaction: {trigger_tokens: 1},
      statecontext: {enabled: true, trigger: ["compaction"], tools: ["task"], role: "developer"},
      tool_definition_paths: [join(process.cwd(), "../sample/tools/native/task.json")],
      paths: {statecontext: "snapshots/statecontext.json"},
      tools: {native: {task: {storage_dir: ".tasks"}}},
    }));
    const configured = await new SessionConfigLoader(sessionDir).load();
    const taskPolicy = configured.settings.tools.native.task!;
    const taskStore = new TaskStore(taskPolicy.storage_dir);
    const task = await taskStore.create({title: "Keep the feature in progress", status: "in-progress", priority: "high", tags: [], documents: []}, taskPolicy);
    const requests: Array<Record<string, unknown>> = [];
    let sequence = 0;
    const fakeClient = {
      responses: {create: async (request: unknown) => {
        requests.push(request as Record<string, unknown>);
        sequence += 1;
        const reasoning = {type: "reasoning", summary: [{type: "summary_text", text: "reasoning ".repeat(100)}]};
        const item = {type: "message", role: "assistant", content: [{type: "output_text", text: `answer-${sequence}`}]};
        return (async function* () {
          yield {type: "response.output_item.done", output_index: 0, item: reasoning};
          yield {type: "response.output_item.done", output_index: 1, item};
          yield completedEvent([reasoning, item], `resp-auto-${sequence}`);
        })();
      }},
    } as unknown as OpenAI;
    const session = await AgentSession.open(sessionDir, {client: fakeClient});
    let usageChangeCount = 0;
    await session.run("first request", {onUsageChanged: () => { usageChangeCount += 1; }});
    assert.equal(usageChangeCount, 1);
    const usageAfterCompaction = await session.readUsageSummary();
    assert.equal(usageAfterCompaction.input_tokens, 10);
    assert.equal(usageAfterCompaction.output_tokens, 4);
    assert.equal(usageAfterCompaction.total_tokens, 14);
    assert.equal(usageAfterCompaction.current?.input_tokens, 0);
    assert.equal(usageAfterCompaction.current?.output_tokens, 0);
    assert.equal(usageAfterCompaction.current?.total_tokens, 0);
    assert.equal(await readFile(join(sessionDir, "messages.jsonl"), "utf8"), "");
    assert.match(await readFile(join(sessionDir, "snapshots/statecontext.json"), "utf8"), /Keep the feature in progress/);
    await taskStore.update(task.id, {title: "Replaced after the next compaction"}, taskPolicy);
    await session.run("second request");
    const secondInput = requests[1]!.input as Array<Record<string, unknown>>;
    assert.equal(secondInput.length, 4);
    assert.deepEqual(secondInput.map((item) => item.role), ["user", "assistant", "user", "developer"]);
    assert.match(JSON.stringify(secondInput[3]), /Keep the feature in progress/);
    await session.run("third request");
    const thirdInput = requests[2]!.input as Array<Record<string, unknown>>;
    assert.equal(thirdInput.at(-1)?.role, "developer");
    assert.match(JSON.stringify(thirdInput.at(-1)), /Replaced after the next compaction/);
    assert.doesNotMatch(JSON.stringify(thirdInput.at(-1)), /Keep the feature in progress/);
    assert.equal(await readFile(join(sessionDir, "messages.jsonl"), "utf8"), "");
    await session.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("refreshes statecontext after tool results from an auto-compacted call", async () => {
  const sessionDir = await makeSession();
  try {
    await writeFile(join(sessionDir, "model.json"), JSON.stringify({model: "gpt-test"}));
    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({
      cwd: sessionDir,
      compaction: {trigger_tokens: 1},
      statecontext: {enabled: true, trigger: ["compaction"], tools: ["task"], role: "developer"},
      tool_definition_paths: [join(process.cwd(), "../sample/tools/native/task.json")],
      tools: {native: {task: {storage_dir: ".tasks"}}},
    }));
    const configured = await new SessionConfigLoader(sessionDir).load();
    const taskPolicy = configured.settings.tools.native.task!;
    const task = await new TaskStore(taskPolicy.storage_dir).create({title: "Transition this task", status: "in-progress", priority: "normal", tags: [], documents: []}, taskPolicy);
    const seed = await SessionResponseStore.open(sessionDir);
    await seed.appendInputItem({type: "message", role: "user", content: [{type: "input_text", text: "older question"}]});
    const oldTurn = await seed.beginTurn("statecontext-old-turn");
    const oldFinal = {type: "message", role: "assistant", content: [{type: "output_text", text: "older answer"}]};
    await oldTurn.stageOutputItem({type: "response.output_item.done", output_index: 0, item: oldFinal});
    await oldTurn.commit(completedEvent([oldFinal], "resp-statecontext-old"));
    await seed.close();
    const requests: Array<Record<string, unknown>> = [];
    let sequence = 0;
    const fakeClient = {
      responses: {create: async (request: unknown) => {
        requests.push(request as Record<string, unknown>);
        sequence += 1;
        const item = sequence === 1
          ? {type: "function_call", call_id: "complete-task", name: "task", arguments: JSON.stringify({action: "done", id: task.id})}
          : {type: "message", role: "assistant", content: [{type: "output_text", text: "Task completed."}]};
        const event = completedEvent([item], `resp-statecontext-${sequence}`);
        if (sequence > 1) event.response.usage = {input_tokens: 0, output_tokens: 1};
        return (async function* () {
          yield {type: "response.output_item.done", output_index: 0, item};
          yield event;
        })();
      }},
    } as unknown as OpenAI;
    const session = await AgentSession.open(sessionDir, {client: fakeClient});
    await session.run("complete the task");
    const input = requests[1]!.input as Array<Record<string, unknown>>;
    const outputIndex = input.findIndex((item) => item.type === "function_call_output");
    const snapshotIndex = input.findIndex((item) => item.role === "developer" && typeof item.content === "object");
    assert.ok(outputIndex >= 0);
    assert.ok(snapshotIndex > outputIndex);
    const snapshotContent = input[snapshotIndex]!.content as Array<{text: string}>;
    assert.match(snapshotContent[0]!.text, /\"status\": \"done\"/);
    await session.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("runs consecutive session turns using config and reconstructed history", async () => {
  const sessionDir = await makeSession();
  try {
    await writeFile(
      join(sessionDir, "model.json"),
      JSON.stringify({ model: "gpt-test", request: { max_output_tokens: 64 } }),
    );
    await writeFile(join(sessionDir, "base.md"), "Be concise.");
    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({ cwd: sessionDir, prompt_documents: ["base.md"], prompt_role: "developer" }),
    );

    const requests: unknown[] = [];
    let responseNumber = 0;
    const fakeClient = {
      responses: {
        create: async (request: unknown) => {
          requests.push(request);
          responseNumber += 1;
          const item = {
            type: "message",
            role: "assistant",
            content: [{ type: "output_text", text: `reply-${responseNumber}` }],
          };
          return (async function* () {
            yield { type: "response.output_item.done", output_index: 0, item };
            yield completedEvent([item], `resp_runner_${responseNumber}`);
          })();
        },
      },
    } as unknown as OpenAI;

    const session = await AgentSession.open(sessionDir, { client: fakeClient });
    const first = await session.run("first", { turnId: "turn-first" });
    const second = await session.run("second", { turnId: "turn-second" });
    assert.equal(first.response.event.response.id, "resp_runner_1");
    assert.equal(first.response.event.session_turn, 1);
    assert.equal(first.response.event.session_step, 1);
    assert.equal(second.response.event.response.id, "resp_runner_2");
    assert.equal(second.response.event.session_turn, 2);
    assert.equal(second.response.event.session_step, 1);

    const firstRequest = requests[0] as Record<string, unknown>;
    const secondRequest = requests[1] as Record<string, unknown>;
    assert.equal(firstRequest.model, "gpt-test");
    assert.equal(firstRequest.stream, true);
    assert.equal(firstRequest.instructions, undefined);
    const firstInput = firstRequest.input as Array<Record<string, unknown>>;
    assert.deepEqual(firstInput[0], {role: "developer", content: [{type: "input_text", text: "Be concise."}]});
    assert.equal(firstInput.length, 2);
    assert.equal((secondRequest.input as unknown[]).length, 4);

    const sessionMessages = (await readFile(join(sessionDir, "messages.jsonl"), "utf8"))
      .trimEnd()
      .split("\n");
    assert.equal(sessionMessages.length, 4);
    const persistedResponses = sessionMessages
      .map((line) => JSON.parse(line) as Record<string, unknown>)
      .filter((record) => record.type === "response.completed");
    assert.deepEqual(persistedResponses.map(({session_turn, session_step}) => [session_turn, session_step]), [
      [1, 1],
      [2, 1],
    ]);
    const usage = await session.readUsageSummary();
    assert.equal(usage.completed_responses, 2);
    assert.equal(usage.turn_count, 2);
    assert.equal(usage.current?.turn, 2);
    assert.equal(usage.current?.step, 1);
    await session.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("transcript reads emit boundary-crossing records once, with unchanged offsets", async () => {
  const root = await makeSession();
  try {
    const path = join(root, "messages.jsonl");
    // The reader refills one 64 KiB buffer per read, so a record longer than
    // that is carried into the next read (a committed response echo is ~77 KB).
    // It must come back byte for byte, exactly once, and the records after it
    // must keep their true offsets.
    const lineOf = (marker: string, bytes: number): string => {
      const bare = JSON.stringify({marker}).length;
      return JSON.stringify({marker, pad: "p".repeat(bytes - (bare + 9))});
    };
    const build = (lines: readonly string[]): {source: string; offsets: number[]} => {
      const offsets: number[] = [];
      let cursor = 0;
      for (const value of lines) {
        offsets.push(cursor);
        cursor += Buffer.byteLength(value) + 1;
      }
      return {source: lines.map((value) => `${value}\n`).join(""), offsets};
    };
    const readBack = async (lines: readonly string[]) => {
      const {source, offsets} = build(lines);
      await writeFile(path, source);
      const readOffsets: number[] = [];
      const readLines: string[] = [];
      await readTranscriptRecords(path, 0, Buffer.byteLength(source), (offset, record) => {
        readOffsets.push(offset);
        readLines.push(record);
      });
      return {offsets, readOffsets, readLines};
    };

    const echo = lineOf("echo", 76_999);
    const outputs = [lineOf("c0", 1_000), lineOf("c1", 1_262), lineOf("c2", 469), lineOf("c3", 390)];
    const source = [lineOf("user", 156), echo, ...outputs, lineOf("r1", 46_288)];
    const first = await readBack(source);
    assert.deepEqual(first.readOffsets, first.offsets);
    assert.deepEqual(first.readLines, source);

    // A record several buffers long crosses the boundary repeatedly.
    const deepSource = [lineOf("user", 100), lineOf("huge", 200_000), lineOf("tail", 500)];
    const deep = await readBack(deepSource);
    assert.deepEqual(deep.readOffsets, deep.offsets);
    assert.deepEqual(deep.readLines, deepSource);
  } finally {
    await removeSession(root);
  }
});
