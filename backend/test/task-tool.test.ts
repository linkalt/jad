import assert from "node:assert/strict";
import {mkdir, mkdtemp, readFile, rm, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import test from "node:test";

import {SessionConfigLoader, type NativeTaskToolSettings} from "../src/config.js";
import {executeNativeTask, normalizeTaskArguments} from "../src/tools/native/task.js";
import {TaskStore} from "../src/tools/native/task-store.js";

const policy: NativeTaskToolSettings = {
  max_tasks: 10,
  list_limit: 5,
  max_list_limit: 10,
  max_title_chars: 100,
  max_tags: 4,
  max_tag_chars: 20,
  max_documents: 4,
  max_document_chars: 100,
  max_storage_bytes: 10_000,
  max_list_bytes: 1_000,
  storage_dir: ".tasks",
  statecontext: {statuses: ["in-progress", "done"], limit: 25},
};

function taskId(text: string): string {
  const match = /^task_id: (T-\d+)$/m.exec(text);
  assert.ok(match);
  return match[1]!;
}

async function makeRunner(overrides: Partial<NativeTaskToolSettings> = {}) {
  const directory = await mkdtemp(join(tmpdir(), "agent-task-tool-"));
  const store = new TaskStore(join(directory, "tasks"));
  let sequence = 0;
  const run = (args: Record<string, unknown>) => executeNativeTask(args, {
    callId: `call-${sequence += 1}`,
    workspaceRoot: directory,
    sessionDir: directory,
    policy: {...policy, ...overrides},
    store,
  });
  return {directory, store, run};
}

test("normalizes task settings and task storage paths", async () => {
  const directory = await mkdtemp(join(tmpdir(), "agent-task-config-"));
  try {
    const workspace = join(directory, "workspace");
    await mkdir(workspace);
    await writeFile(join(directory, "setting.json"), JSON.stringify({
      cwd: workspace,
      tools: {native: {task: {
        max_tasks: 12,
        list_limit: 2,
        max_list_limit: 3,
        storage_dir: "plans",
      }}}, 
    }));
    const loader = new SessionConfigLoader(directory);
    const settings = await loader.loadSettings();
    assert.equal(settings.tools.native.task?.max_tasks, 12);
    assert.equal(settings.tools.native.task?.list_limit, 2);
    assert.equal(settings.tools.native.task?.storage_dir, join(workspace, "plans"));
    const absolute = join(directory, "absolute-tasks");
    await writeFile(join(directory, "setting.json"), JSON.stringify({cwd: workspace, tools: {native: {task: {storage_dir: absolute}}}}));
    assert.equal((await new SessionConfigLoader(directory).loadSettings()).tools.native.task?.storage_dir, absolute);
    await writeFile(join(directory, "setting.json"), JSON.stringify({cwd: workspace, tools: {native: {task: {storage_path: "tasks.json"}}}}));
    await assert.rejects(new SessionConfigLoader(directory).loadSettings(), /storage_path was removed; use storage_dir/);
  } finally {
    await rm(directory, {recursive: true, force: true});
  }
});

test("adds, updates, completes, lists, and persists documented tasks", async () => {
  const {directory, run} = await makeRunner();
  try {
    const added = await run({title: "  Draft task design  ", tags: ["Tools", "tools"], documents: ["design_task.md", "design_task.md"]});
    const first = taskId(added);
    assert.match(added, /status: todo/);
    assert.match(added, /tags: \["tools"\]/);
    assert.match(added, /documents: \["design_task.md"\]/);

    const active = taskId(await run({title: "Implement it", status: "in progress", priority: "HIGH"}));
    const changed = await run({id: active, status: "blocked", documents: ["design_task.md", "docs/plan.md"]});
    assert.match(changed, /status: blocked/);
    assert.match(changed, /documents: \["design_task.md","docs\/plan.md"\]/);

    const listed = await run({});
    assert.match(listed, new RegExp(first));
    assert.match(listed, new RegExp(active));
    assert.match(listed, /documents: 2/);
    const completed = await run({action: "complete", taskId: first});
    assert.match(completed, /status: done/);
    const done = await run({status: "done"});
    assert.match(done, new RegExp(first));
    const fetched = await run({id: active});
    assert.match(fetched, /docs\/plan.md/);
    const stored = JSON.parse(await readFile(join(directory, "tasks", "tasks.json"), "utf8")) as {tasks: Array<{id: string}>};
    assert.deepEqual(stored.tasks.map((task) => task.id), [first, active]);
  } finally {
    await rm(directory, {recursive: true, force: true});
  }
});

test("limits creation states and requires explicit destructive deletion", async () => {
  const {directory, run} = await makeRunner();
  try {
    assert.match(await run({title: "Cannot start blocked", status: "blocked"}), /error_code: invalid_creation_status/);
    assert.match(await run({title: "Cannot start done", status: "done"}), /error_code: invalid_creation_status/);
    const id = taskId(await run({title: "Delete only explicitly"}));
    assert.match(await run({id, action: "delete"}), /Task deleted/);
    assert.match(await run({id}), /error_code: task_not_found/);
    assert.match(await run({action: "delete"}), /error_code: missing_task_id/);
  } finally {
    await rm(directory, {recursive: true, force: true});
  }
});

test("bounds task count, documents, and list output without corrupting storage", async () => {
  const {directory, run} = await makeRunner({max_tasks: 1, max_list_bytes: 20});
  try {
    const id = taskId(await run({title: "One task", documents: ["a.md"]}));
    assert.match(await run({title: "Too many"}), /error_code: task_storage_error/);
    const list = await run({status: "all"});
    assert.match(list, /has_more: true/);
    assert.doesNotMatch(list, /--- CONTENT/);
    assert.match(await run({id, documents: ["a.md", "b.md", "c.md", "d.md", "e.md"]}), /error_code: invalid_documents/);
    const before = await readFile(join(directory, "tasks", "tasks.json"), "utf8");
    await writeFile(join(directory, "tasks", "tasks.json"), "not-json");
    assert.match(await run({title: "Unsafe overwrite"}), /error_code: task_storage_error/);
    assert.equal(await readFile(join(directory, "tasks", "tasks.json"), "utf8"), "not-json");
    assert.notEqual(before, "not-json");
  } finally {
    await rm(directory, {recursive: true, force: true});
  }
});

test("normalizes compatible task aliases", () => {
  assert.deepEqual(normalizeTaskArguments({title: "New task"}), {title: "New task", action: "add"});
  assert.deepEqual(normalizeTaskArguments({action: "COMPLETE", taskId: " t-001 "}), {action: "done", id: "T-001"});
  assert.deepEqual(normalizeTaskArguments({status: "IN PROGRESS"}), {status: "in-progress", action: "list"});
});
