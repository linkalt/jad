import assert from "node:assert/strict";
import {execFileSync} from "node:child_process";
import {chmod, mkdir, mkdtemp, readFile, readlink, rm, stat, symlink, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import test from "node:test";

import {
  CheckpointError,
  CheckpointService,
  callChangesWorkspace,
  createRunCheckpoint,
  exclusionPatterns,
} from "../src/checkpoint.js";
import {
  DEFAULT_CHECKPOINT_MAX_CHECKPOINTS,
  DEFAULT_CHECKPOINT_MAX_FILES,
  DEFAULT_CHECKPOINT_MAX_TOTAL_BYTES,
  DEFAULT_CHECKPOINT_TIMEOUT_MS,
  type CheckpointSettings,
} from "../src/config.js";

function settings(overrides: Partial<CheckpointSettings> = {}): CheckpointSettings {
  return {
    enabled: true,
    git_binary: "git",
    max_checkpoints: DEFAULT_CHECKPOINT_MAX_CHECKPOINTS,
    max_files: DEFAULT_CHECKPOINT_MAX_FILES,
    max_total_bytes: DEFAULT_CHECKPOINT_MAX_TOTAL_BYTES,
    timeout_ms: DEFAULT_CHECKPOINT_TIMEOUT_MS,
    include_mcp_tools: true,
    snapshot_on: "first_mutation",
    safe_restore: true,
    ...overrides,
  };
}

async function fixture(overrides: Partial<CheckpointSettings> = {}) {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-checkpoint-"));
  const workspace = join(root, "ws");
  const staging = join(root, "session", "staging");
  await mkdir(workspace, {recursive: true});
  await mkdir(staging, {recursive: true});
  const harnessPaths = [join(workspace, ".tasks")];
  const service = new CheckpointService({
    settings: settings(overrides),
    workspace,
    stagingDir: staging,
    harnessPaths,
    durability: "process",
  });
  return {
    root,
    workspace,
    staging,
    service,
    async cleanup() {
      await rm(root, {recursive: true, force: true});
    },
  };
}

async function seed(workspace: string): Promise<void> {
  await writeFile(join(workspace, "a.txt"), "a0\n");
  await mkdir(join(workspace, "d"), {recursive: true});
  await writeFile(join(workspace, "d", "b.txt"), "b0\n");
  await writeFile(join(workspace, "run.sh"), "#!/bin/sh\n");
  await chmod(join(workspace, "run.sh"), 0o755);
  await symlink("a.txt", join(workspace, "link"));
}

const read = (workspace: string, name: string) => readFile(join(workspace, name), "utf8");
const exists = (path: string) => stat(path).then(() => true, () => false);

test("a safe restore reverts edits, deletions, new files, modes and symlinks of one run", async () => {
  const f = await fixture();
  try {
    await seed(f.workspace);
    const id = await f.service.begin("turn-1", 1);
    assert.ok(id, "a checkpoint id");
    await writeFile(join(f.workspace, "a.txt"), "a1\n");
    await rm(join(f.workspace, "d", "b.txt"));
    await writeFile(join(f.workspace, "new.txt"), "new\n");
    await mkdir(join(f.workspace, "made", "deep"), {recursive: true});
    await writeFile(join(f.workspace, "made", "deep", "x.txt"), "x\n");
    await chmod(join(f.workspace, "run.sh"), 0o644);
    await rm(join(f.workspace, "link"));
    await symlink("d", join(f.workspace, "link"));
    await f.service.finish(id);

    const result = await f.service.restore(id, {dryRun: false});
    assert.equal(result.mode, "safe");
    assert.deepEqual(result.conflicts, []);
    assert.equal(await read(f.workspace, "a.txt"), "a0\n");
    assert.equal(await read(f.workspace, "d/b.txt"), "b0\n");
    assert.equal(await exists(join(f.workspace, "new.txt")), false);
    assert.equal(await exists(join(f.workspace, "made", "deep", "x.txt")), false);
    assert.equal((await stat(join(f.workspace, "run.sh"))).mode & 0o777, 0o755);
    assert.equal(await readlink(join(f.workspace, "link")), "a.txt");
    assert.ok(result.undo_checkpoint_id);
  } finally {
    await f.cleanup();
  }
});

test("a dry run lists the paths and changes nothing, and writes no checkpoint", async () => {
  const f = await fixture();
  try {
    await seed(f.workspace);
    const id = (await f.service.begin("turn-1", 1)) as string;
    await writeFile(join(f.workspace, "a.txt"), "a1\n");
    await writeFile(join(f.workspace, "new.txt"), "new\n");
    await f.service.finish(id);
    const listed = (await f.service.list()).length;

    const result = await f.service.restore(id, {dryRun: true});
    assert.deepEqual(result.restored, ["a.txt"]);
    assert.deepEqual(result.deleted, ["new.txt"]);
    assert.equal(result.dry_run, true);
    assert.equal(result.undo_checkpoint_id, undefined);
    assert.equal(await read(f.workspace, "a.txt"), "a1\n");
    assert.equal(await exists(join(f.workspace, "new.txt")), true);
    assert.equal((await f.service.list()).length, listed);
  } finally {
    await f.cleanup();
  }
});

test("safe restore leaves a file the user changed after the run and reports it", async () => {
  const f = await fixture();
  try {
    await seed(f.workspace);
    const id = (await f.service.begin("turn-1", 1)) as string;
    await writeFile(join(f.workspace, "a.txt"), "agent\n");
    await writeFile(join(f.workspace, "d", "b.txt"), "agent-b\n");
    await f.service.finish(id);
    await writeFile(join(f.workspace, "a.txt"), "user edit after the run\n");

    const result = await f.service.restore(id, {dryRun: false});
    assert.deepEqual(result.conflicts, ["a.txt"]);
    assert.deepEqual(result.restored, ["d/b.txt"]);
    assert.equal(await read(f.workspace, "a.txt"), "user edit after the run\n");
    assert.equal(await read(f.workspace, "d/b.txt"), "b0\n");
  } finally {
    await f.cleanup();
  }
});

test("full restore makes the workspace equal to the state before the run, user edits included", async () => {
  const f = await fixture();
  try {
    await seed(f.workspace);
    const id = (await f.service.begin("turn-1", 1)) as string;
    await writeFile(join(f.workspace, "a.txt"), "agent\n");
    await f.service.finish(id);
    await writeFile(join(f.workspace, "a.txt"), "user\n");
    await writeFile(join(f.workspace, "later.txt"), "later\n");

    const result = await f.service.restore(id, {dryRun: false, mode: "full"});
    assert.equal(result.mode, "full");
    assert.equal(await read(f.workspace, "a.txt"), "a0\n");
    assert.equal(await exists(join(f.workspace, "later.txt")), false);
  } finally {
    await f.cleanup();
  }
});

test("a restore is itself undoable through the checkpoint it records", async () => {
  const f = await fixture();
  try {
    await seed(f.workspace);
    const id = (await f.service.begin("turn-1", 1)) as string;
    await writeFile(join(f.workspace, "a.txt"), "agent\n");
    await writeFile(join(f.workspace, "new.txt"), "new\n");
    await f.service.finish(id);

    const restored = await f.service.restore(id, {dryRun: false});
    assert.equal(await read(f.workspace, "a.txt"), "a0\n");
    const undoId = restored.undo_checkpoint_id as string;
    assert.ok(undoId);
    const undo = (await f.service.list()).find((record) => record.id === undoId);
    assert.equal(undo?.kind, "pre_restore");
    assert.equal(undo?.restored_from, id);

    await f.service.restore(undoId, {dryRun: false, mode: "full"});
    assert.equal(await read(f.workspace, "a.txt"), "agent\n");
    assert.equal(await read(f.workspace, "new.txt"), "new\n");
  } finally {
    await f.cleanup();
  }
});

test("the user's own git repository is untouched by snapshots and restores", async () => {
  const f = await fixture();
  try {
    const git = (...args: string[]) =>
      execFileSync("git", args, {cwd: f.workspace, encoding: "utf8", env: {...process.env, GIT_CONFIG_GLOBAL: "/dev/null"}});
    git("init", "-q");
    await seed(f.workspace);
    git("add", "-A");
    git("-c", "user.email=u@e", "-c", "user.name=u", "commit", "-qm", "init");
    await writeFile(join(f.workspace, "a.txt"), "dirty\n");
    await writeFile(join(f.workspace, "untracked.txt"), "u\n");
    const fingerprint = () => [
      git("status", "--short"),
      git("stash", "list"),
      git("write-tree"),
      git("rev-parse", "HEAD"),
      git("for-each-ref"),
    ].join("|");
    const before = fingerprint();

    const id = (await f.service.begin("turn-1", 1)) as string;
    await writeFile(join(f.workspace, "b2.txt"), "n\n");
    await f.service.finish(id);
    await f.service.restore(id, {dryRun: false});

    assert.equal(await exists(join(f.workspace, "b2.txt")), false);
    assert.equal(fingerprint(), before);
  } finally {
    await f.cleanup();
  }
});

test("ignored files, harness directories and write/edit backups are neither snapshotted nor restored", async () => {
  const f = await fixture();
  try {
    await seed(f.workspace);
    await writeFile(join(f.workspace, ".gitignore"), "build/\n");
    await mkdir(join(f.workspace, "build"), {recursive: true});
    await writeFile(join(f.workspace, "build", "out.js"), "o0\n");
    await mkdir(join(f.workspace, ".tasks"), {recursive: true});
    await writeFile(join(f.workspace, ".tasks", "t.json"), "t0\n");
    const id = (await f.service.begin("turn-1", 1)) as string;
    await writeFile(join(f.workspace, "build", "out.js"), "o1\n");
    await writeFile(join(f.workspace, ".tasks", "t.json"), "t1\n");
    await writeFile(join(f.workspace, "a.txt.bak-1791259056390-8d53b3fdbbc761e8"), "old\n");
    await writeFile(join(f.workspace, "a.txt"), "a1\n");
    await f.service.finish(id);

    const result = await f.service.restore(id, {dryRun: false});
    assert.deepEqual(result.restored, ["a.txt"]);
    assert.deepEqual(result.deleted, []);
    assert.equal(await read(f.workspace, "build/out.js"), "o1\n");
    assert.equal(await read(f.workspace, ".tasks/t.json"), "t1\n");
    assert.equal(await exists(join(f.workspace, "a.txt.bak-1791259056390-8d53b3fdbbc761e8")), true);
  } finally {
    await f.cleanup();
  }
});

test("exclusionPatterns keeps only harness paths inside the workspace and escapes glob characters", () => {
  const patterns = exclusionPatterns("/w/ws", ["/w/ws/.tasks", "/w/ws/a[1]/b", "/elsewhere/x", "/w/ws"]);
  assert.ok(patterns.includes("/.tasks"));
  assert.ok(patterns.includes("/a\\[1\\]/b"));
  assert.ok(!patterns.some((pattern) => pattern.includes("elsewhere")));
  assert.equal(patterns.filter((pattern) => pattern === "/").length, 0);
});

test("a workspace over max_files is skipped with a warning and the run is not blocked", async () => {
  const f = await fixture({max_files: 2});
  try {
    await seed(f.workspace);
    const id = await f.service.begin("turn-1", 1);
    assert.equal(id, undefined);
    const [record] = await f.service.list();
    assert.equal(record?.state, "skipped");
    assert.match(record?.warning ?? "", /max_files/);
    await assert.rejects(f.service.restore(record?.id as string, {dryRun: true}), (error) =>
      error instanceof CheckpointError && error.code === "conflict");
  } finally {
    await f.cleanup();
  }
});

test("a missing git binary is recorded as a failed checkpoint and never throws", async () => {
  const f = await fixture({git_binary: "/nonexistent/git"});
  try {
    await seed(f.workspace);
    const id = await f.service.begin("turn-1", 1);
    assert.equal(id, undefined);
    const [record] = await f.service.list();
    assert.equal(record?.state, "failed");
    assert.match(record?.warning ?? "", /cannot run/);
  } finally {
    await f.cleanup();
  }
});

test("a filesystem root or the home directory is refused as a workspace", async () => {
  const f = await fixture();
  try {
    const rootService = new CheckpointService({
      settings: settings(),
      workspace: "/",
      stagingDir: f.staging,
      harnessPaths: [],
      durability: "process",
    });
    assert.equal(await rootService.begin("turn-1", 1), undefined);
    const [record] = await rootService.list();
    assert.equal(record?.state, "skipped");
    assert.match(record?.warning ?? "", /filesystem root or the home directory/);
  } finally {
    await f.cleanup();
  }
});

test("only max_checkpoints records are kept, oldest pruned first", async () => {
  const f = await fixture({max_checkpoints: 3});
  try {
    await seed(f.workspace);
    const ids: string[] = [];
    for (let turn = 1; turn <= 5; turn += 1) {
      await writeFile(join(f.workspace, "a.txt"), `a${turn}\n`);
      const id = (await f.service.begin(`turn-${turn}`, turn)) as string;
      await f.service.finish(id);
      ids.push(id);
    }
    const listed = (await f.service.list()).map((record) => record.id);
    assert.deepEqual(listed, [ids[4], ids[3], ids[2]]);
    await assert.rejects(f.service.restore(ids[0] as string, {dryRun: true}), (error) =>
      error instanceof CheckpointError && error.code === "not_found");
  } finally {
    await f.cleanup();
  }
});

test("safe restore needs the run-end snapshot; full restore works without it", async () => {
  const f = await fixture();
  try {
    await seed(f.workspace);
    const id = (await f.service.begin("turn-1", 1)) as string;
    await writeFile(join(f.workspace, "a.txt"), "agent\n");
    // The run never finished (for example the server stopped): there is no after_tree.
    await assert.rejects(f.service.restore(id, {dryRun: false}), (error) =>
      error instanceof CheckpointError && error.code === "conflict" && /run-end snapshot/.test(error.message));
    await f.service.restore(id, {dryRun: false, mode: "full"});
    assert.equal(await read(f.workspace, "a.txt"), "a0\n");
  } finally {
    await f.cleanup();
  }
});

test("restoring a checkpoint that does not exist is not_found", async () => {
  const f = await fixture();
  try {
    await assert.rejects(f.service.restore("ckpt-nope", {dryRun: true}), (error) =>
      error instanceof CheckpointError && error.code === "not_found");
  } finally {
    await f.cleanup();
  }
});

test("a damaged checkpoints.json reads as an empty list", async () => {
  const f = await fixture();
  try {
    await writeFile(join(f.staging, "checkpoints.json"), "{not json");
    assert.deepEqual(await f.service.list(), []);
  } finally {
    await f.cleanup();
  }
});

test("callChangesWorkspace is true for write, edit, bash, a process start and MCP tools, and false for reads", () => {
  const call = (name: string, args: unknown = {}) => ({name, arguments: JSON.stringify(args)});
  assert.equal(callChangesWorkspace(call("write"), "native", true), true);
  assert.equal(callChangesWorkspace(call("edit"), "native", true), true);
  assert.equal(callChangesWorkspace(call("bash"), "native", true), true);
  assert.equal(callChangesWorkspace(call("process", {action: "start", command: "x"}), "native", true), true);
  assert.equal(callChangesWorkspace(call("process", {action: "status", id: "p"}), "native", true), false);
  assert.equal(callChangesWorkspace(call("read"), "native", true), false);
  assert.equal(callChangesWorkspace(call("grep"), "native", true), false);
  assert.equal(callChangesWorkspace(call("srv_tool"), "mcp", true), true);
  assert.equal(callChangesWorkspace(call("srv_tool"), "mcp", false), false);
  assert.equal(callChangesWorkspace({name: "process", arguments: "{bad"}, "native", true), false);
});

test("a run takes one checkpoint at its first changing call, none for a read-only run", async () => {
  const f = await fixture();
  try {
    await seed(f.workspace);
    const readOnly = createRunCheckpoint(f.service, "turn-1", 1);
    await readOnly.start();
    await readOnly.beforeCalls(false);
    await readOnly.finish();
    assert.equal((await f.service.list()).length, 0);

    const changing = createRunCheckpoint(f.service, "turn-2", 2);
    await changing.start();
    await changing.beforeCalls(false);
    assert.equal((await f.service.list()).length, 0);
    await changing.beforeCalls(true);
    await changing.beforeCalls(true);
    await changing.finish();
    const records = await f.service.list();
    assert.equal(records.length, 1);
    assert.equal(records[0]?.state, "complete");
  } finally {
    await f.cleanup();
  }
});

test("run_start takes the checkpoint before any call, even a read", async () => {
  const f = await fixture({snapshot_on: "run_start"});
  try {
    await seed(f.workspace);
    const hooks = createRunCheckpoint(f.service, "turn-1", 1);
    await hooks.start();
    assert.equal((await f.service.list()).length, 1);
    await hooks.beforeCalls(true);
    await hooks.finish();
    const records = await f.service.list();
    assert.equal(records.length, 1);
    assert.equal(records[0]?.state, "complete");
  } finally {
    await f.cleanup();
  }
});
