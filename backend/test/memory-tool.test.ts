import assert from "node:assert/strict";
import {createHash} from "node:crypto";
import {Ajv} from "ajv";
import {mkdir, mkdtemp, rm, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import test from "node:test";

import {SessionConfigLoader, type NativeMemoryToolSettings} from "../src/config.js";
import {executeNativeMemory, nativeMemoryDefinition} from "../src/tools/native/memory.js";
import {MemoryStore, MemoryStoreError} from "../src/tools/native/memory-store.js";

const policy: NativeMemoryToolSettings = {
  storage_dir: ".memory", max_types: 4, max_records_per_type: 4, max_total_records: 10,
  max_record_bytes: 10_000, max_total_bytes: 50_000, list_limit: 2, max_list_limit: 4,
  max_list_bytes: 10_000, max_scan_records: 20, max_scan_bytes: 50_000,
  max_filter_fields: 4, max_filter_depth: 4, max_json_depth: 8,
  max_cursors: 8, cursor_ttl_ms: 60_000, max_cursor_bytes: 10_000,
  statecontext: [],
};

function legacyCursor(type: string, order: "newest" | "oldest", last: string): string {
  const filter = createHash("sha256").update("{}").digest("hex");
  return Buffer.from(JSON.stringify({v: 1, type, order, filter, last})).toString("base64url");
}

async function runner() {
  const directory = await mkdtemp(join(tmpdir(), "agent-memory-tool-"));
  const store = new MemoryStore(join(directory, "memory")); let sequence = 0;
  const run = (args: Record<string, unknown>) => executeNativeMemory(args, {callId: `call-${sequence += 1}`, workspaceRoot: directory, sessionDir: directory, policy, store});
  return {directory, store, run};
}

test("normalizes memory storage relative to cwd and accepts absolute storage", async () => {
  const directory = await mkdtemp(join(tmpdir(), "agent-memory-config-"));
  try {
    const workspace = join(directory, "workspace"); await mkdir(workspace);
    await writeFile(join(directory, "setting.json"), JSON.stringify({cwd: workspace, tools: {native: {memory: {storage_dir: "state/memory", allowed_types: ["task"]}}}}));
    const settings = await new SessionConfigLoader(directory).loadSettings();
    assert.equal(settings.tools.native.memory?.storage_dir, join(workspace, "state/memory"));
    assert.deepEqual(settings.tools.native.memory?.allowed_types, ["task"]);
    assert.equal(settings.tools.native.memory?.max_cursors, 1_000);
    assert.equal(settings.tools.native.memory?.cursor_ttl_ms, 86_400_000);
    const absolute = join(directory, "absolute-memory");
    await writeFile(join(directory, "setting.json"), JSON.stringify({cwd: workspace, tools: {native: {memory: {storage_dir: absolute}}}}));
    assert.equal((await new SessionConfigLoader(directory).loadSettings()).tools.native.memory?.storage_dir, absolute);
    await writeFile(join(directory, "setting.json"), JSON.stringify({cwd: workspace, tools: {native: {memory: {storage_path: "memory"}}}}));
    await assert.rejects(new SessionConfigLoader(directory).loadSettings(), /storage_path was removed; use storage_dir/);
  } finally { await rm(directory, {recursive: true, force: true}); }
});

test("creates, gets, filters, and ETag-updates dynamic memory records", async () => {
  const {directory, store, run} = await runner();
  try {
    const created = await store.create("task", {title: "one", status: "open", project: {name: "core", flags: {a: 1, b: 2}}}, policy);
    const second = await store.create("task", {title: "two", status: "done"}, policy);
    const listed = await store.list("task", {"project.name": "core", status: "open"}, 2, "newest", undefined, policy);
    assert.deepEqual(listed.items.map((item) => item.ref), [created.ref]);
    assert.deepEqual((await store.list("task", {project: {flags: {b: 2, a: 1}, name: "core"}}, 2, "newest", undefined, policy)).items.map((item) => item.ref), [created.ref]);
    const fetched = await store.get("task", created.ref, policy);
    const updated = await store.update("task", created.ref, fetched.etag, {title: "one", status: "done"}, policy);
    assert.notEqual(updated.etag, fetched.etag);
    await assert.rejects(store.update("task", created.ref, fetched.etag, {title: "stale"}, policy), MemoryStoreError);
    assert.match(await run({operation: "get", type: "task", ref: created.ref}), /"status": "done"/);
    const updateResult = await run({operation: "update", type: "task", ref: created.ref, data: {title: "without-etag"}});
    assert.match(updateResult, /Memory record updated/);
    assert.ok(updateResult.includes(`ref: ${created.ref}`));
    assert.doesNotMatch(updateResult, /--- CONTENT token=/);
    const savedUpdate = await store.get("task", created.ref, policy);
    assert.ok(updateResult.includes(`etag: ${savedUpdate.etag}`));
    assert.notEqual(savedUpdate.etag, fetched.etag);
    assert.deepEqual(savedUpdate.data, {title: "without-etag"});
    assert.match(await run({operation: "list", type: "task", filter: {status: "done"}}), /two/);
    assert.ok(second.ref.startsWith("task/"));
  } finally { await rm(directory, {recursive: true, force: true}); }
});

test("paginates without loss, defaults an omitted cursor order, and persists same-millisecond insertion order", async () => {
  const directory = await mkdtemp(join(tmpdir(), "agent-memory-pagination-"));
  try {
    const root = join(directory, "memory");
    const fixedNow = () => new Date("2026-01-01T00:00:00.000Z");
    const firstStore = new MemoryStore(root, fixedNow);
    const created = [
      await firstStore.create("page", {id: 1}, policy),
      await firstStore.create("page", {id: 2}, policy),
    ];
    const secondStore = new MemoryStore(root, fixedNow);
    created.push(await secondStore.create("page", {id: 3}, policy), await secondStore.create("page", {id: 4}, policy));

    const newestFirst = await secondStore.list("page", {}, 2, "newest", undefined, policy);
    assert.deepEqual(newestFirst.items.map((item) => item.ref), [created[3]!.ref, created[2]!.ref]);
    assert.match(newestFirst.nextCursor!, /^m_[0-9a-f-]{36}$/);
    assert.doesNotMatch(newestFirst.nextCursor!, /page|newest/);
    const newestSecond = await secondStore.list("page", undefined, 2, undefined, newestFirst.nextCursor, policy);
    assert.deepEqual(newestSecond.items.map((item) => item.ref), [created[1]!.ref, created[0]!.ref]);
    assert.equal(newestSecond.nextCursor, undefined);
    assert.deepEqual((await secondStore.list("page", {}, 2, undefined, legacyCursor("page", "newest", created[1]!.ref), policy)).items.map((item) => item.ref), [created[1]!.ref, created[0]!.ref]);

    const oldestFirst = await secondStore.list("page", {}, 3, "oldest", undefined, policy);
    assert.deepEqual(oldestFirst.items.map((item) => item.ref), [created[0]!.ref, created[1]!.ref, created[2]!.ref]);
    assert.ok(oldestFirst.nextCursor, "a limit+1 record must remain reachable");
    const oldestSecond = await secondStore.list("page", {}, 3, undefined, oldestFirst.nextCursor, policy);
    assert.deepEqual(oldestSecond.items.map((item) => item.ref), [created[3]!.ref]);
    await assert.rejects(secondStore.list("page", {}, 2, "newest", oldestFirst.nextCursor, policy), /cursor is invalid/);
    const missing = "page/page-20260101T000000.000Z-0000000000009999-00000000-0000-4000-8000-000000000000.json";
    await assert.rejects(secondStore.list("page", {}, 2, undefined, legacyCursor("page", "newest", missing), policy), /cursor position was not found/);
    const filtered = [await secondStore.create("filtered", {kind: "keep", id: 1}, policy), await secondStore.create("filtered", {kind: "keep", id: 2}, policy)];
    const filteredFirst = await secondStore.list("filtered", {kind: "keep"}, 1, "oldest", undefined, policy);
    assert.deepEqual(filteredFirst.items.map((item) => item.ref), [filtered[0]!.ref]);
    assert.deepEqual((await secondStore.list("filtered", undefined, 1, undefined, filteredFirst.nextCursor, policy)).items.map((item) => item.ref), [filtered[1]!.ref]);
  } finally { await rm(directory, {recursive: true, force: true}); }
});

test("declares optional optimistic-concurrency for updates in its schema", () => {
  const validate = new Ajv().compile(nativeMemoryDefinition.inputSchema);
  assert.equal(validate({operation: "update", type: "task", ref: "task/ref", data: {value: 1}}), true);
  assert.equal(validate({operation: "update", type: "task", ref: "task/ref", if_match: "sha256:value", data: {value: 1}}), true);
});

test("rejects unsafe types, refs, and unsupported operation fields", async () => {
  const {directory, run} = await runner();
  try {
    assert.match(await run({operation: "create", type: "../escape", data: {x: 1}}), /memory type is invalid/);
    assert.match(await run({operation: "get", type: "task", ref: "task/../../etc/passwd"}), /memory ref is invalid/);
    assert.match(await run({operation: "delete", type: "task", ref: "x"}), /invalid_operation/);
    assert.match(await run({operation: "list", type: "task", filter: {"bad..path": 1}}), /memory filter exceeds/);
  } finally { await rm(directory, {recursive: true, force: true}); }
});
