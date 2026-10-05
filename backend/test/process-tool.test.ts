import assert from "node:assert/strict";
import {mkdir, mkdtemp, readFile, rm, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import test from "node:test";

import {SessionConfigLoader, type NativeProcessToolSettings} from "../src/config.js";
import {executeNativeProcess, normalizeProcessArguments} from "../src/tools/native/process.js";
import {ProcessManager} from "../src/tools/native/process-manager.js";

const policy: NativeProcessToolSettings = {
  shell: "/bin/bash",
  login_shell: false,
  kill_grace_ms: 100,
  max_running: 4,
  max_completed: 10,
  retention_days: 14,
  wait_timeout_ms: 100,
  max_wait_timeout_ms: 2_000,
  stdout: {head_bytes: 32, tail_bytes: 32},
  stderr: {head_bytes: 32, tail_bytes: 32},
  log_cap_bytes: 4_096,
  max_line_chars: 200,
  strip_ansi: true,
  env: {NO_COLOR: "1", TERM: "dumb"},
  env_deny: [],
  storage_dir: ".processes",
  paths: [],
  unrestricted_paths: false,
  statecontext: {states: ["running"], limit: 20},
};

function resultId(text: string): string {
  const match = /^process_id: (proc_[^\n]+)$/m.exec(text);
  assert.ok(match);
  return match[1]!;
}

async function makeRunner(
  overrides: Partial<NativeProcessToolSettings> = {},
  environment: NodeJS.ProcessEnv = process.env,
) {
  const appliedPolicy: NativeProcessToolSettings = {...policy, ...overrides};
  const directory = await mkdtemp(join(tmpdir(), "agent-process-tool-"));
  const manager = new ProcessManager(join(directory, "processes"));
  await manager.recover();
  let sequence = 0;
  const run = async (args: Record<string, unknown>) => executeNativeProcess(args, {
    callId: `call-${sequence += 1}`,
    workspaceRoot: directory,
    sessionDir: directory,
    policy: appliedPolicy,
    manager,
    apiKeyEnv: "PROCESS_TOOL_TEST_SECRET",
    environment,
  });
  return {directory, manager, run, policy: appliedPolicy};
}

test("normalizes process settings and process storage paths", async () => {
  const directory = await mkdtemp(join(tmpdir(), "agent-process-config-"));
  try {
    const workspace = join(directory, "workspace");
    await mkdir(workspace);
    await writeFile(join(directory, "setting.json"), JSON.stringify({
      cwd: workspace,
      tools: {native: {process: {
        storage_dir: join(directory, "runtime-processes"),
        max_running: 2,
        max_completed: 3,
        retention_days: 1,
        stdout: {head_bytes: 12, tail_bytes: 24},
        stderr: {head_bytes: 6, tail_bytes: 18},
      }}},
    }));
    const loader = new SessionConfigLoader(directory);
    const settings = await loader.loadSettings();
    assert.equal(settings.tools.native.process?.max_running, 2);
    assert.equal(settings.tools.native.process?.max_completed, 3);
    assert.deepEqual(settings.tools.native.process?.stdout, {head_bytes: 12, tail_bytes: 24});
    assert.deepEqual(settings.tools.native.process?.stderr, {head_bytes: 6, tail_bytes: 18});
    assert.equal(settings.tools.native.process?.storage_dir, join(directory, "runtime-processes"));
  } finally {
    await rm(directory, {recursive: true, force: true});
  }
});

test("loads process env_file relative to the session with inline overrides and denials", async () => {
  const directory = await mkdtemp(join(tmpdir(), "agent-process-env-file-"));
  try {
    await writeFile(join(directory, "process.env"), [
      "# process-specific configuration",
      "FILE_VALUE=from-file",
      "OVERRIDDEN=from-file",
      "QUOTED=\"line one\\nline two # literal\"",
      "export DENIED=from-file",
      "",
    ].join("\n"));
    await writeFile(join(directory, "setting.json"), JSON.stringify({
      cwd: directory,
      tools: {native: {process: {
        env_file: "process.env",
        env: {OVERRIDDEN: "inline"},
        env_deny: ["DENIED"],
      }}},
    }));
    const settings = await new SessionConfigLoader(directory).loadSettings();
    const loaded = settings.tools.native.process!;
    assert.equal(loaded.env_file, join(directory, "process.env"));
    assert.equal(loaded.env.NO_COLOR, "1");
    const {manager, run} = await makeRunner({env: loaded.env, env_deny: loaded.env_deny});
    try {
      const started = await run({
        command: "printf '%s|%s|%s|%s' \"$FILE_VALUE\" \"$OVERRIDDEN\" \"$QUOTED\" \"${DENIED-unset}\"",
      });
      const completed = await run({action: "wait", id: resultId(started), timeout_ms: 1_000});
      assert.match(completed, /from-file\|inline\|line one\nline two # literal\|unset/);
    } finally {
      await manager.close();
    }
  } finally {
    await rm(directory, {recursive: true, force: true});
  }
});

test("loads an absolute process env_file and rejects missing or malformed files", async () => {
  const directory = await mkdtemp(join(tmpdir(), "agent-process-env-file-"));
  try {
    const envFile = join(directory, "absolute.env");
    await writeFile(envFile, "ABSOLUTE_VALUE=available\n");
    const configure = async (value: unknown) => {
      await writeFile(join(directory, "setting.json"), JSON.stringify({
        cwd: directory,
        tools: {native: {process: {env_file: value}}},
      }));
      return new SessionConfigLoader(directory).loadSettings();
    };
    const settings = await configure(envFile);
    assert.equal(settings.tools.native.process?.env_file, envFile);
    assert.equal(settings.tools.native.process?.env.ABSOLUTE_VALUE, "available");

    await assert.rejects(configure("missing.env"), /tools\.native\.process\.env_file: could not read/);
    await assert.rejects(configure("   "), /env_file/);
    await writeFile(join(directory, "bad.env"), "not an assignment\n");
    await assert.rejects(configure("bad.env"), /bad\.env:1: expected NAME=value/);
  } finally {
    await rm(directory, {recursive: true, force: true});
  }
});

test("starts, waits for, lists, and reports bounded process logs", async () => {
  const {directory, manager, run} = await makeRunner();
  try {
    const started = await run({command: "printf ready; sleep 0.05; printf done"});
    const id = resultId(started);
    const waiting = await run({action: "wait", id, timeout_ms: 1_000});
    assert.match(waiting, /waited: true/);
    assert.match(waiting, /state: exited/);
    assert.match(waiting, /ready/);
    assert.match(waiting, /done/);

    const listed = await run({action: "list", filter: "exited"});
    assert.match(listed, new RegExp(id));
    assert.doesNotMatch(listed, /--- CONTENT/);
    const status = await run({id});
    assert.match(status, /state: exited/);
  } finally {
    await manager.close();
    await rm(directory, {recursive: true, force: true});
  }
});

test("concurrent starts cannot exceed max_running and refused starts free their slot", async () => {
  const {directory, manager, run} = await makeRunner({max_running: 2});
  try {
    // Parallel tool calls reach start() together; every one of them used to pass
    // the capacity check before any of them had registered a process.
    const results = await Promise.all(
      Array.from({length: 6}, () => run({command: "sleep 30"}).catch((error: unknown) => String(error))),
    );
    const started = results.filter((text) => /^process_id: proc_/m.test(text));
    assert.equal(started.length, 2, `exactly max_running processes start: ${JSON.stringify(results.map((text) => text.slice(0, 60)))}`);
    const listed = await run({action: "list", filter: "running"});
    assert.equal((listed.match(/proc_/g) ?? []).length, 2);

    // The four refused starts must not have leaked a reserved slot: once a
    // process ends, a new one is accepted again.
    await run({action: "stop", id: resultId(started[0]!)});
    const again = await run({command: "sleep 30"});
    assert.match(again, /^process_id: proc_/m);
  } finally {
    await manager.close();
    await rm(directory, {recursive: true, force: true});
  }
});

test("does not expose the HTTP API token to managed child processes", async () => {
  const {directory, manager, run} = await makeRunner({}, {...process.env, AGENT_API_TOKEN: "never-inherit-this-secret"});
  try {
    const started = await run({command: "test -z \"${AGENT_API_TOKEN-}\" && printf clean"});
    const id = resultId(started);
    const completed = await run({action: "wait", id, timeout_ms: 1_000});
    assert.match(completed, /clean/);
    assert.doesNotMatch(completed, /leaked|never-inherit-this-secret/);
  } finally {
    await manager.close();
    await rm(directory, {recursive: true, force: true});
  }
});

test("returns separate stdout and stderr head and tail excerpts", async () => {
  const {directory, manager, run} = await makeRunner({
    stdout: {head_bytes: 16, tail_bytes: 16},
    stderr: {head_bytes: 16, tail_bytes: 16},
  });
  try {
    const started = await run({command: "printf stdout-head; printf '%*s' 96 '' | tr ' ' x; printf stdout-tail; printf stderr-head >&2; printf '%*s' 96 '' | tr ' ' y >&2; printf stderr-tail >&2"});
    const id = resultId(started);
    const completed = await run({action: "wait", id, timeout_ms: 1_000});
    assert.match(completed, /\[stdout\]/);
    assert.match(completed, /stdout-head/);
    assert.match(completed, /stdout-tail/);
    assert.match(completed, /\[stderr\]/);
    assert.match(completed, /stderr-head/);
    assert.match(completed, /stderr-tail/);
    assert.match(completed, /bytes omitted/);
  } finally {
    await manager.close();
    await rm(directory, {recursive: true, force: true});
  }
});

test("returns a running status on wait timeout and stops the managed process group", async () => {
  const {directory, manager, run} = await makeRunner();
  try {
    const started = await run({command: "sleep 10"});
    const id = resultId(started);
    const timed = await run({action: "wait", id, timeout_ms: 1});
    assert.match(timed, /waited: false/);
    assert.match(timed, /state: running/);
    const stopped = await run({action: "stop", id});
    assert.match(stopped, /state: stopped/);
    const repeat = await run({action: "stop", id});
    assert.match(repeat, /state: stopped/);
  } finally {
    await manager.close();
    await rm(directory, {recursive: true, force: true});
  }
});

test("does not bind a managed process to its originating run abort signal", async () => {
  const {directory, manager} = await makeRunner();
  try {
    const controller = new AbortController();
    const started = await executeNativeProcess({command: "sleep 10"}, {
      callId: "process-run-abort",
      workspaceRoot: directory,
      sessionDir: directory,
      policy,
      manager,
      apiKeyEnv: "PROCESS_TOOL_TEST_SECRET",
      environment: process.env,
      signal: controller.signal,
    });
    const id = resultId(started);
    controller.abort("provider_run_complete");
    assert.equal(manager.snapshot(id)?.state, "running");
    await manager.stop(id);
  } finally {
    await manager.close();
    await rm(directory, {recursive: true, force: true});
  }
});

test("retains only the configured number of completed process records", async () => {
  const {directory, manager, run} = await makeRunner({max_completed: 1});
  try {
    const first = resultId(await run({command: "true"}));
    const firstWait = await manager.wait(first, 1_000);
    assert.equal(firstWait.waited, true);
    assert.equal(firstWait.snapshot?.state, "exited");
    const second = resultId(await run({command: "true"}));
    const secondWait = await manager.wait(second, 1_000);
    assert.equal(secondWait.waited, true);
    assert.equal(secondWait.snapshot?.state, "exited");
    const listed = await run({action: "list", filter: "exited"});
    assert.match(listed, new RegExp(second));
    assert.doesNotMatch(listed, new RegExp(first));
  } finally {
    await manager.close();
    await rm(directory, {recursive: true, force: true});
  }
});

test("wait does not report completion until retention pruning finishes", async () => {
  const {directory, manager, run} = await makeRunner({max_completed: 1});
  let releasePrune!: () => void;
  let signalPrune!: () => void;
  const blockedPrune = new Promise<void>((resolve) => { releasePrune = resolve; });
  const pruneEntered = new Promise<void>((resolve) => { signalPrune = resolve; });
  const internals = manager as unknown as {
    prune: (maxCompleted: number, retentionDays: number) => Promise<void>;
  };
  const originalPrune = internals.prune.bind(manager);
  internals.prune = async (maxCompleted, retentionDays) => {
    signalPrune();
    await blockedPrune;
    await originalPrune(maxCompleted, retentionDays);
  };
  try {
    const id = resultId(await run({command: "true"}));
    await pruneEntered;
    const whilePruning = await manager.wait(id, 10);
    assert.equal(whilePruning.waited, false);
    releasePrune();
    const finalized = await manager.wait(id, 1_000);
    assert.equal(finalized.waited, true);
    assert.equal(finalized.snapshot?.state, "exited");
  } finally {
    releasePrune();
    internals.prune = originalPrune;
    await manager.close();
    await rm(directory, {recursive: true, force: true});
  }
});

test("normalizes process aliases and marks unclean-running records lost on recovery", async () => {
  assert.deepEqual(normalizeProcessArguments({action: "KILL", id: " abc "}), {action: "stop", id: "abc"});
  assert.deepEqual(normalizeProcessArguments({filter: "ALL"}), {action: "list"});

  const {directory, manager, run} = await makeRunner();
  try {
    const started = await run({command: "printf recovered-stdout-head; printf '%*s' 96 '' | tr ' ' x; printf recovered-stdout-tail; printf recovered-stderr-head >&2; printf '%*s' 96 '' | tr ' ' y >&2; printf recovered-stderr-tail >&2; sleep 10"});
    const id = resultId(started);
    await manager.wait(id, 50);
    const recovered = new ProcessManager(join(directory, "processes"));
    await recovered.recover();
    const snapshot = await recovered.snapshotWithLogs(
      id,
      {headBytes: policy.stdout.head_bytes, tailBytes: policy.stdout.tail_bytes},
      {headBytes: policy.stderr.head_bytes, tailBytes: policy.stderr.tail_bytes},
      policy.max_line_chars,
      policy.strip_ansi,
    );
    assert.equal(snapshot?.state, "lost");
    assert.match(snapshot?.output.text ?? "", /\[stdout\]/);
    assert.match(snapshot?.output.text ?? "", /recovered-stdout-head/);
    assert.match(snapshot?.output.text ?? "", /recovered-stdout-tail/);
    assert.match(snapshot?.output.text ?? "", /\[stderr\]/);
    assert.match(snapshot?.output.text ?? "", /recovered-stderr-head/);
    assert.match(snapshot?.output.text ?? "", /recovered-stderr-tail/);
    assert.match(snapshot?.output.text ?? "", /bytes omitted/);
    const manifest = await readFile(join(directory, "processes", id, "manifest.json"), "utf8");
    assert.match(manifest, /backend_restarted/);
  } finally {
    await manager.close();
    await rm(directory, {recursive: true, force: true});
  }
});
