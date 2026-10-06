import assert from "node:assert/strict";
import {chmod, mkdir, mkdtemp, readFile, rm, stat, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import test from "node:test";

import type OpenAI from "openai";

import {createAgentServer} from "../src/server.js";
import {nativeReadDefinition} from "../src/tools/native/read.js";
import {nativeWriteDefinition} from "../src/tools/native/write.js";

const TEST_API_TOKEN = "test-api-token-0123456789abcdef0123456789abcdef";
const AUTH = {authorization: `Bearer ${TEST_API_TOKEN}`, "content-type": "application/json"};

type ProviderOutput = Record<string, unknown>;
type Step = ProviderOutput[] | Error;
interface SseEvent {event: string; data: Record<string, unknown>}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function frameEvent(block: string): SseEvent | undefined {
  let event = "message";
  let data = "";
  for (const line of block.split("\n")) {
    if (line.startsWith("event:")) event = line.slice(6).trim();
    else if (line.startsWith("data:")) data += line.slice(5).trim();
  }
  if (data.length === 0) return undefined;
  return {event, data: JSON.parse(data) as Record<string, unknown>};
}

function sseReader(reader: ReadableStreamDefaultReader<Uint8Array>) {
  const decoder = new TextDecoder();
  let buffered = "";
  return {
    async next(): Promise<SseEvent> {
      for (;;) {
        const boundary = buffered.indexOf("\n\n");
        if (boundary >= 0) {
          const block = buffered.slice(0, boundary);
          buffered = buffered.slice(boundary + 2);
          const event = frameEvent(block);
          if (event !== undefined) return event;
          continue;
        }
        const next = await reader.read();
        if (next.done) throw new Error("run stream ended before the expected event");
        buffered += decoder.decode(next.value, {stream: true});
      }
    },
  };
}

async function untilDone(events: ReturnType<typeof sseReader>): Promise<void> {
  for (;;) {
    const event = await events.next();
    if (event.event === "done" || event.event === "error") return;
  }
}

function completedEvent(output: ProviderOutput[], id: string): ProviderOutput {
  return {
    type: "response.completed",
    response: {id, object: "response", status: "completed", model: "fake-model", usage: {input_tokens: 10, output_tokens: 4}, output},
  };
}

const writeCall = (callId: string, path: string, content: string): ProviderOutput => ({
  id: `fc-${callId}`, type: "function_call", call_id: callId, name: "write", arguments: JSON.stringify({path, content}),
});
const readCall = (callId: string, path: string): ProviderOutput => ({
  id: `fc-${callId}`, type: "function_call", call_id: callId, name: "read", arguments: JSON.stringify({path}),
});
const finalMessage = (text: string): ProviderOutput => ({
  id: "final", type: "message", role: "assistant", content: [{type: "output_text", text}],
});

interface FixtureOptions {
  steps: Step[];
  checkpoint?: Record<string, unknown>;
  approvalOnWrite?: boolean;
  /** Put the session directory inside the workspace, like a project that keeps its sessions under .jad. */
  nested?: boolean;
}

async function createFixture(options: FixtureOptions) {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-checkpoint-run-"));
  const workspace = join(root, "ws");
  const sessionsDir = options.nested === true ? join(workspace, ".jad", "sessions") : join(root, "sessions");
  const sessionDir = join(sessionsDir, "session1");
  await mkdir(workspace, {recursive: true});
  await mkdir(sessionDir, {recursive: true});
  await writeFile(
    join(sessionDir, "model.json"),
    JSON.stringify({schema_version: 1, provider: "fake", api_key_env: "FAKE_API_KEY", model: "fake-model"}),
  );
  await writeFile(
    join(sessionDir, "setting.json"),
    JSON.stringify({
      schema_version: 1,
      session_name: "checkpoint-run",
      cwd: workspace,
      tool_definition_paths: [join(root, "tools.json")],
      tools: {
        native: {
          read: {paths: {allow: [workspace]}},
          write: {paths: {allow: [workspace]}, ...(options.approvalOnWrite === true ? {approval: "ask"} : {})},
        },
      },
      ...(options.checkpoint === undefined ? {} : {checkpoint: options.checkpoint}),
    }),
  );
  await writeFile(
    join(root, "tools.json"),
    JSON.stringify({schema_version: 1, tools: [nativeReadDefinition, nativeWriteDefinition]}),
  );
  let providerCalls = 0;
  const fakeClient = {
    responses: {
      create: async () => {
        const step = options.steps[providerCalls];
        assert.ok(step, `unexpected provider request ${providerCalls + 1}`);
        providerCalls += 1;
        if (step instanceof Error) throw step;
        return (async function* () {
          for (const [output_index, item] of step.entries()) {
            yield {type: "response.output_item.done", output_index, item};
          }
          yield completedEvent(step, `response-${providerCalls}`);
        })();
      },
    },
  } as unknown as OpenAI;
  const running = createAgentServer({sessionsDir, client: fakeClient, apiToken: TEST_API_TOKEN});
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });
  const address = running.server.address();
  assert.ok(address !== null && typeof address !== "string");
  const baseUrl = `http://127.0.0.1:${address.port}`;
  const api = (path: string, init: RequestInit = {}) =>
    fetch(`${baseUrl}/api/sessions/session1${path}`, {...init, headers: {...AUTH, ...(init.headers ?? {})}});
  return {
    root,
    workspace,
    sessionDir,
    baseUrl,
    api,
    /** Runs one turn to its end: "done" for a run that completed, "error" for one that failed. */
    async runToEnd(): Promise<"done" | "error"> {
      const response = await api("/run", {method: "POST", body: JSON.stringify({input: "go"})});
      assert.equal(response.status, 200);
      const text = await response.text();
      const events = text.split("\n\n").map(frameEvent).filter((event): event is SseEvent => event !== undefined);
      // A failed run sends `error` and then `done`, so `error` anywhere decides it.
      return events.some((event) => event.event === "error") ? "error" : "done";
    },
    async checkpoints(): Promise<Array<Record<string, unknown>>> {
      const response = await api("/checkpoints");
      assert.equal(response.status, 200);
      return ((await response.json()) as {checkpoints: Array<Record<string, unknown>>}).checkpoints;
    },
    async close() {
      await running.close();
      await new Promise<void>((resolve) => running.server.close(() => resolve()));
      await rm(root, {recursive: true, force: true});
    },
  };
}

const exists = (path: string) => stat(path).then(() => true, () => false);

test("checkpoints are off by default: a write runs, nothing is recorded and no files are created", async () => {
  const f = await createFixture({steps: [[writeCall("w1", "a.txt", "hello")], [finalMessage("done")]]});
  try {
    assert.equal(await f.runToEnd(), "done");
    assert.equal(await readFile(join(f.workspace, "a.txt"), "utf8"), "hello");
    const list = await (await f.api("/checkpoints")).json() as {enabled: boolean; checkpoints: unknown[]};
    assert.deepEqual(list, {enabled: false, restoring: false, checkpoints: []});
    assert.equal(await exists(join(f.sessionDir, "staging", "checkpoints.json")), false);
    assert.equal(await exists(join(f.sessionDir, "staging", "checkpoints.git")), false);
  } finally {
    await f.close();
  }
});

test("a run that writes takes one checkpoint and the user can list, dry-run and restore it", async () => {
  const f = await createFixture({
    checkpoint: {enabled: true},
    steps: [[writeCall("w1", "a.txt", "agent")], [writeCall("w2", "b.txt", "second")], [finalMessage("done")]],
  });
  try {
    await writeFile(join(f.workspace, "a.txt"), "original");
    assert.equal(await f.runToEnd(), "done");
    assert.equal(await readFile(join(f.workspace, "a.txt"), "utf8"), "agent");

    const records = await f.checkpoints();
    assert.equal(records.length, 1, "one checkpoint for the whole run, not one per write");
    const [record] = records;
    assert.equal(record?.state, "complete");
    assert.equal(record?.kind, "run");
    const id = record?.id as string;

    const dry = await f.api(`/checkpoints/${id}/restore`, {method: "POST", body: JSON.stringify({dry_run: true})});
    assert.equal(dry.status, 200);
    const planned = (await dry.json() as {restore: {restored: string[]; deleted: string[]; dry_run: boolean}}).restore;
    assert.deepEqual(planned.restored, ["a.txt"]);
    assert.deepEqual(planned.deleted, ["b.txt"]);
    assert.equal(await readFile(join(f.workspace, "a.txt"), "utf8"), "agent", "a dry run changes nothing");

    const done = await f.api(`/checkpoints/${id}/restore`, {method: "POST", body: JSON.stringify({})});
    assert.equal(done.status, 200);
    assert.equal(await readFile(join(f.workspace, "a.txt"), "utf8"), "original");
    assert.equal(await exists(join(f.workspace, "b.txt")), false);
    const after = await f.checkpoints();
    assert.equal(after.length, 2);
    assert.equal(after[0]?.kind, "pre_restore", "the restore is recorded so it can be undone");
  } finally {
    await f.close();
  }
});

test("a read-only run takes no checkpoint", async () => {
  const f = await createFixture({
    checkpoint: {enabled: true},
    steps: [[readCall("r1", "a.txt")], [finalMessage("done")]],
  });
  try {
    await writeFile(join(f.workspace, "a.txt"), "x");
    assert.equal(await f.runToEnd(), "done");
    assert.deepEqual(await f.checkpoints(), []);
    assert.equal(await exists(join(f.sessionDir, "staging", "checkpoints.git")), false);
  } finally {
    await f.close();
  }
});

test("a run that fails after changing files still records its end state", async () => {
  const f = await createFixture({
    checkpoint: {enabled: true},
    steps: [[writeCall("w1", "a.txt", "agent")], new Error("provider unreachable")],
  });
  try {
    await writeFile(join(f.workspace, "a.txt"), "original");
    assert.equal(await f.runToEnd(), "error");
    const [record] = await f.checkpoints();
    assert.equal(record?.state, "complete");
    assert.ok(record?.after_tree, "the end of a failed run is recorded");
    const restore = await f.api(`/checkpoints/${record?.id as string}/restore`, {method: "POST", body: JSON.stringify({})});
    assert.equal(restore.status, 200);
    assert.equal(await readFile(join(f.workspace, "a.txt"), "utf8"), "original");
  } finally {
    await f.close();
  }
});

test("a snapshot that cannot be taken does not stop the tool call and is recorded", async () => {
  const f = await createFixture({
    checkpoint: {enabled: true, git_binary: "/nonexistent/git"},
    steps: [[writeCall("w1", "a.txt", "hello")], [finalMessage("done")]],
  });
  try {
    assert.equal(await f.runToEnd(), "done");
    assert.equal(await readFile(join(f.workspace, "a.txt"), "utf8"), "hello");
    const [record] = await f.checkpoints();
    assert.equal(record?.state, "failed");
    assert.match(String(record?.warning), /cannot run/);
    const restore = await f.api(`/checkpoints/${record?.id as string}/restore`, {method: "POST", body: JSON.stringify({})});
    assert.equal(restore.status, 409);
  } finally {
    await f.close();
  }
});

test("a session directory inside the workspace is never part of a snapshot or a restore", async () => {
  const f = await createFixture({
    nested: true,
    checkpoint: {enabled: true},
    steps: [[writeCall("w1", "a.txt", "agent")], [finalMessage("done")]],
  });
  try {
    await writeFile(join(f.workspace, "a.txt"), "original");
    assert.equal(await f.runToEnd(), "done");
    const messagesBefore = await readFile(join(f.sessionDir, "messages.jsonl"), "utf8");
    const [record] = await f.checkpoints();
    const dry = await f.api(`/checkpoints/${record?.id as string}/restore`, {method: "POST", body: JSON.stringify({dry_run: true})});
    const planned = (await dry.json() as {restore: {restored: string[]; deleted: string[]; conflicts: string[]}}).restore;
    assert.deepEqual([...planned.restored, ...planned.deleted, ...planned.conflicts], ["a.txt"]);
    const done = await f.api(`/checkpoints/${record?.id as string}/restore`, {method: "POST", body: JSON.stringify({})});
    assert.equal(done.status, 200);
    assert.equal(await readFile(join(f.sessionDir, "messages.jsonl"), "utf8"), messagesBefore, "the transcript is not rolled back");
  } finally {
    await f.close();
  }
});

test("restore is refused with 409 while a run is active, and the file is left alone", async () => {
  const f = await createFixture({
    checkpoint: {enabled: true},
    approvalOnWrite: true,
    steps: [[writeCall("w1", "a.txt", "agent")], [finalMessage("done")]],
  });
  let reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
  try {
    await writeFile(join(f.workspace, "a.txt"), "original");
    const run = await f.api("/run", {method: "POST", body: JSON.stringify({input: "go", supports_interaction: true})});
    reader = run.body!.getReader();
    const events = sseReader(reader);
    let pending: Record<string, unknown> | undefined;
    while (pending === undefined) {
      const event = await events.next();
      if (event.event === "interaction" && isRecord(event.data.pending)) pending = event.data.pending;
    }
    // The run is parked on the approval; its checkpoint was taken before the question.
    const [record] = await f.checkpoints();
    assert.equal(record?.state, "open");
    const restore = await f.api(`/checkpoints/${record?.id as string}/restore`, {method: "POST", body: JSON.stringify({mode: "full"})});
    assert.equal(restore.status, 409);
    assert.match(String((await restore.json() as {error: string}).error), /run is active/);

    const answer = await f.api("/run/interaction", {method: "POST", body: JSON.stringify({interaction_id: pending.id, decision: "accept"})});
    assert.equal(answer.status, 200);
    await untilDone(events);
    assert.equal((await f.checkpoints())[0]?.state, "complete");
  } finally {
    await reader?.cancel().catch(() => undefined);
    await f.close();
  }
});

test("the checkpoint routes answer 404, 400 and 401 like the other session routes", async () => {
  const f = await createFixture({checkpoint: {enabled: true}, steps: []});
  try {
    const missing = await f.api("/checkpoints/ckpt-nope/restore", {method: "POST", body: JSON.stringify({})});
    assert.equal(missing.status, 404);
    for (const body of [{mode: "everything"}, {dry_run: "yes"}, {surprise: 1}, []]) {
      const bad = await f.api("/checkpoints/ckpt-nope/restore", {method: "POST", body: JSON.stringify(body)});
      assert.equal(bad.status, 400, JSON.stringify(body));
    }
    const noToken = await fetch(`${f.baseUrl}/api/sessions/session1/checkpoints`);
    assert.equal(noToken.status, 401);
    const wrongMethod = await f.api("/checkpoints", {method: "POST", body: "{}"});
    assert.equal(wrongMethod.status, 404);
  } finally {
    await f.close();
  }
});

/** Rewrites the stored records the way a server killed mid-run leaves them: still open, no run-end snapshot. */
async function leaveOpen(sessionDir: string, kinds: Array<"run" | "pre_restore"> = ["run", "pre_restore"]): Promise<void> {
  const path = join(sessionDir, "staging", "checkpoints.json");
  const document = JSON.parse(await readFile(path, "utf8")) as {checkpoints: Array<Record<string, unknown>>};
  for (const record of document.checkpoints) {
    if (!kinds.includes(record.kind as "run" | "pre_restore")) continue;
    record.state = "open";
    delete record.after_tree;
    delete record.finished_at;
    delete record.warning;
  }
  await writeFile(path, JSON.stringify(document));
}

test("a run checkpoint left open by a stopped server is listed as interrupted and a full restore still works", async () => {
  const f = await createFixture({
    checkpoint: {enabled: true},
    steps: [[writeCall("w1", "a.txt", "agent")], [finalMessage("done")]],
  });
  try {
    await writeFile(join(f.workspace, "a.txt"), "original");
    assert.equal(await f.runToEnd(), "done");
    await leaveOpen(f.sessionDir);
    const storedBefore = await readFile(join(f.sessionDir, "staging", "checkpoints.json"), "utf8");

    const [record] = await f.checkpoints();
    assert.equal(record?.state, "interrupted", "no run is live, so an open record was cut short");
    assert.equal(record?.warning, "The run ended without recording its end (the server stopped or crashed).");
    assert.ok(record?.before_tree, "the saved state is kept");
    assert.equal(
      await readFile(join(f.sessionDir, "staging", "checkpoints.json"), "utf8"),
      storedBefore,
      "listing never writes the record back",
    );

    const id = record?.id as string;
    const safe = await f.api(`/checkpoints/${id}/restore`, {method: "POST", body: JSON.stringify({mode: "safe"})});
    assert.equal(safe.status, 409);
    assert.match(String((await safe.json() as {error: string}).error), /use mode "full"/);
    const full = await f.api(`/checkpoints/${id}/restore`, {method: "POST", body: JSON.stringify({mode: "full"})});
    assert.equal(full.status, 200);
    assert.equal(await readFile(join(f.workspace, "a.txt"), "utf8"), "original");
  } finally {
    await f.close();
  }
});

test("a pre_restore checkpoint left open is reported the same way, and finished records are untouched", async () => {
  const f = await createFixture({
    checkpoint: {enabled: true},
    steps: [[writeCall("w1", "a.txt", "agent")], [finalMessage("done")]],
  });
  try {
    await writeFile(join(f.workspace, "a.txt"), "original");
    assert.equal(await f.runToEnd(), "done");
    const [run] = await f.checkpoints();
    const restored = await f.api(`/checkpoints/${run?.id as string}/restore`, {method: "POST", body: JSON.stringify({})});
    assert.equal(restored.status, 200);
    assert.deepEqual((await f.checkpoints()).map((record) => [record.kind, record.state]), [
      ["pre_restore", "complete"],
      ["run", "complete"],
    ]);

    await leaveOpen(f.sessionDir, ["pre_restore"]);
    assert.deepEqual((await f.checkpoints()).map((record) => [record.kind, record.state]), [
      ["pre_restore", "interrupted"],
      ["run", "complete"],
    ]);
  } finally {
    await f.close();
  }
});

test("a checkpoint is open, not interrupted, while its run is live", async () => {
  const f = await createFixture({
    checkpoint: {enabled: true},
    approvalOnWrite: true,
    steps: [[writeCall("w1", "a.txt", "agent")], [finalMessage("done")]],
  });
  let reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
  try {
    await writeFile(join(f.workspace, "a.txt"), "original");
    const run = await f.api("/run", {method: "POST", body: JSON.stringify({input: "go", supports_interaction: true})});
    reader = run.body!.getReader();
    const events = sseReader(reader);
    let pending: Record<string, unknown> | undefined;
    while (pending === undefined) {
      const event = await events.next();
      if (event.event === "interaction" && isRecord(event.data.pending)) pending = event.data.pending;
    }
    assert.equal((await f.checkpoints())[0]?.state, "open");
    const answer = await f.api("/run/interaction", {method: "POST", body: JSON.stringify({interaction_id: pending.id, decision: "accept"})});
    assert.equal(answer.status, 200);
    await untilDone(events);
    assert.equal((await f.checkpoints())[0]?.state, "complete");
  } finally {
    await reader?.cancel().catch(() => undefined);
    await f.close();
  }
});

/**
 * A git that waits while `flag` exists, so a restore can be held open long enough
 * to look at it; without the flag it is plain git, so a run's own snapshots are fast.
 */
async function slowGit(root: string, flag: string): Promise<string> {
  const script = join(root, "slow-git.sh");
  await writeFile(script, `#!/bin/sh\nif [ -e "${flag}" ]; then sleep 0.12; fi\nexec git "$@"\n`);
  await chmod(script, 0o755);
  return script;
}

async function untilTrue(check: () => Promise<boolean>, what: string): Promise<void> {
  const deadline = Date.now() + 15_000;
  while (Date.now() < deadline) {
    if (await check()) return;
    await new Promise((resolve) => setTimeout(resolve, 25));
  }
  assert.fail(`timed out waiting for ${what}`);
}

test("the list says restoring while a real restore runs, not for a dry run, and a second restore is refused", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-slow-git-"));
  const flag = join(root, "slow.flag");
  const git = await slowGit(root, flag);
  const f = await createFixture({
    checkpoint: {enabled: true, git_binary: git},
    steps: [[writeCall("w1", "a.txt", "agent")], [finalMessage("done")]],
  });
  const listed = async () => (await (await f.api("/checkpoints")).json()) as {restoring: boolean};
  try {
    await writeFile(join(f.workspace, "a.txt"), "original");
    assert.equal(await f.runToEnd(), "done");
    const [record] = await f.checkpoints();
    const id = record?.id as string;
    assert.equal((await listed()).restoring, false, "nothing is restoring before the restore starts");

    await writeFile(flag, "slow");
    // A dry run changes nothing, so it is not reported even while it is slow.
    const dry = f.api(`/checkpoints/${id}/restore`, {method: "POST", body: JSON.stringify({dry_run: true})});
    await new Promise((resolve) => setTimeout(resolve, 60));
    assert.equal((await listed()).restoring, false, "a dry run is not a restore");
    assert.equal((await dry).status, 200);

    const restore = f.api(`/checkpoints/${id}/restore`, {method: "POST", body: JSON.stringify({})});
    await untilTrue(async () => (await listed()).restoring, "the list to report the restore");
    assert.equal(await readFile(join(f.workspace, "a.txt"), "utf8"), "agent", "the restore has not finished yet");

    const second = await f.api(`/checkpoints/${id}/restore`, {method: "POST", body: JSON.stringify({})});
    assert.equal(second.status, 409, "a second restore is refused while the first runs");
    assert.equal((await listed()).restoring, true, "a refused second restore does not clear the first");

    const done = await restore;
    assert.equal(done.status, 200);
    assert.equal(await readFile(join(f.workspace, "a.txt"), "utf8"), "original");
    assert.equal((await listed()).restoring, false, "the flag clears when the restore ends");
  } finally {
    await rm(flag, {force: true});
    await f.close();
    await rm(root, {recursive: true, force: true});
  }
});

test("a restore that is refused does not leave the list reporting restoring", async () => {
  const f = await createFixture({
    checkpoint: {enabled: true},
    steps: [[writeCall("w1", "a.txt", "agent")], [finalMessage("done")]],
  });
  try {
    assert.equal(await f.runToEnd(), "done");
    const missing = await f.api("/checkpoints/ckpt-nope/restore", {method: "POST", body: JSON.stringify({})});
    assert.equal(missing.status, 404);
    const list = (await (await f.api("/checkpoints")).json()) as {restoring: boolean};
    assert.equal(list.restoring, false);
  } finally {
    await f.close();
  }
});
