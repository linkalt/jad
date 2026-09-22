import assert from "node:assert/strict";
import {execFileSync} from "node:child_process";
import {access, mkdir, mkdtemp, readdir, readFile, realpath, rm, utimes, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import test from "node:test";

import {SessionConfigLoader} from "../src/config.js";
import {loadToolCatalog} from "../src/tools/index.js";
import {effectiveTimeoutMs} from "../src/tools/native/bash.js";
import {cutLongLines, OutputCollector, SpillWriter, stripControlCharacters} from "../src/tools/native/child.js";
import {
  firstLine,
  informationFields,
  makeBashToolSession,
  outputText,
  refreshBashSession,
  type NativeBashSessionOptions,
} from "./tool-session.js";

const bashSkip = ((): false | string => {
  try {
    execFileSync("bash", ["--version"], {stdio: "ignore"});
    return false;
  } catch {
    return "bash is not installed";
  }
})();

type BashRunner = (callId: string, args: Record<string, unknown>) => Promise<string>;

async function openBashSession(
  options: NativeBashSessionOptions = {},
  model: {api_key_env?: string} = {},
) {
  const sessionDir = await makeBashToolSession(options);
  if (model.api_key_env !== undefined) {
    // The credential variable is resolved when the catalog is built, so the
    // model document must be final before the tool is loaded.
    await writeFile(
      join(sessionDir, "model.json"),
      JSON.stringify({model: "gpt-test", api_key_env: model.api_key_env}),
    );
  }
  const catalog = await loadToolCatalog(await new SessionConfigLoader(sessionDir).load());
  const run: BashRunner = async (callId, args) =>
    outputText(
      (
        await catalog.executeFunctionCall(
          {
            type: "function_call",
            call_id: callId,
            name: "bash",
            arguments: JSON.stringify(args),
          },
          {workspaceRoot: sessionDir, sessionDir},
        )
      ).output,
    );
  return {sessionDir, run};
}

async function exists(path: string): Promise<boolean> {
  try {
    await access(path);
    return true;
  } catch {
    return false;
  }
}

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

test("runs the smallest call and reports where it ran", {skip: bashSkip}, async () => {
  const {sessionDir, run} = await openBashSession();
  try {
    const output = await run("bash-echo", {command: "echo hello-from-bash"});
    assert.equal(firstLine(output).status, "ok");
    assert.match(output, /"summary":"Command completed in \d+ ms"/);
    const fields = informationFields(output);
    assert.equal(fields.cwd, ".");
    assert.equal(fields.exit_code, "0");
    assert.equal(fields.truncated, "false");
    assert.ok(Number(fields.stdout_bytes) > 0);
    assert.equal(fields.stderr_bytes, "0");
    assert.doesNotMatch(output, /error_code/);
    assert.match(output, /--- CONTENT token=[\w-]+ encoding=utf8 ---/);
    assert.match(output, /hello-from-bash/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("returns metadata only when a command prints nothing", {skip: bashSkip}, async () => {
  const {sessionDir, run} = await openBashSession();
  try {
    const output = await run("bash-silent", {command: "true"});
    assert.equal(firstLine(output).status, "ok");
    const fields = informationFields(output);
    assert.equal(fields.stdout_bytes, "0");
    assert.equal(fields.stderr_bytes, "0");
    assert.doesNotMatch(output, /--- CONTENT token=/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("reports a non-zero exit as an error with the output intact", {skip: bashSkip}, async () => {
  const {sessionDir, run} = await openBashSession();
  try {
    const output = await run("bash-exit", {command: "echo failing-anyway; exit 3"});
    assert.equal(firstLine(output).status, "error");
    assert.match(output, /"summary":"Command exited with 3"/);
    const fields = informationFields(output);
    assert.equal(fields.error_type, "execution");
    assert.equal(fields.error_code, "nonzero_exit");
    assert.equal(fields.exit_code, "3");
    assert.match(output, /failing-anyway/);
    assert.doesNotMatch(output, /hint:/);

    const missing = await run("bash-127", {command: "command-that-does-not-exist-xyz"});
    assert.equal(firstLine(missing).status, "error");
    assert.equal(informationFields(missing).exit_code, "127");
    assert.match(informationFields(missing).hint ?? "", /not found/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("attributes stdout and stderr and keeps both in one stream", {skip: bashSkip}, async () => {
  const {sessionDir, run} = await openBashSession();
  try {
    const output = await run("bash-both", {
      command: "printf 'to-out\\n'; printf 'to-err\\n' 1>&2",
    });
    assert.equal(firstLine(output).status, "ok");
    const fields = informationFields(output);
    assert.ok(Number(fields.stdout_bytes) >= 7);
    assert.ok(Number(fields.stderr_bytes) >= 7);
    assert.ok(Number(fields.stdout_shown_bytes) >= 7);
    assert.match(output, /to-out[\s\S]*to-err/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("strips control characters unless the session disables it", {skip: bashSkip}, async () => {
  const coloured = "printf '\\033[31mred\\033[0m\\n'";
  const stripped = await openBashSession();
  try {
    const output = await stripped.run("bash-ansi-on", {command: coloured});
    assert.match(output, /red/);
    assert.equal(output.includes("\u001b"), false);
  } finally {
    await rm(stripped.sessionDir, {recursive: true, force: true});
  }

  const raw = await openBashSession({strip_ansi: false});
  try {
    const output = await raw.run("bash-ansi-off", {command: coloured});
    assert.equal(output.includes("\u001b[31mred"), true);
  } finally {
    await rm(raw.sessionDir, {recursive: true, force: true});
  }
});

test("cuts long lines and keeps the end of a large stream", {skip: bashSkip}, async () => {
  const {sessionDir, run} = await openBashSession({
    max_line_chars: 100,
    max_bytes: 4096,
    stdout: {head_bytes: 2048, tail_bytes: 2048},
    stderr: {head_bytes: 0, tail_bytes: 0},
  });
  try {
    const longLine = await run("bash-long-line", {command: "printf 'x%.0s' {1..500}; echo"});
    assert.equal(firstLine(longLine).status, "ok");
    assert.match(longLine, /\[\.\.\. truncated line\]/);
    assert.doesNotMatch(longLine, new RegExp("x".repeat(200)));

    const flood = await run("bash-flood", {command: "seq 1 20000"});
    const fields = informationFields(flood);
    assert.equal(fields.truncated, "true");
    assert.equal(fields.truncation_reason, "max_bytes");
    assert.ok(Number(fields.omitted_bytes) > 0);
    assert.match(flood, /\[\.\.\. \d+ bytes omitted \.\.\.\]/);
    assert.match(flood, /1\n2\n3\n/);
    assert.match(flood, /20000\n+--- END CONTENT/);
    assert.ok(Number(fields.returned_bytes) < 4096 + 512);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("retains only the tail when the head budget is zero", {skip: bashSkip}, async () => {
  const {sessionDir, run} = await openBashSession({
    max_bytes: 64,
    stdout: {head_bytes: 0, tail_bytes: 64},
    stderr: {head_bytes: 0, tail_bytes: 0},
  });
  try {
    const output = await run("bash-tail-only", {command: "seq 1 500"});
    assert.equal(firstLine(output).status, "ok");
    assert.match(output, /\[\.\.\. \d+ bytes omitted \.\.\.\]/);
    assert.match(output, /500\n+--- END CONTENT/);
    assert.doesNotMatch(output, /1\n2\n3/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("keeps memory bounded for a very large stream", {skip: bashSkip}, async () => {
  const {sessionDir, run} = await openBashSession({
    max_bytes: 8192,
    stdout: {head_bytes: 4096, tail_bytes: 4096},
    stderr: {head_bytes: 0, tail_bytes: 0},
  });
  try {
    const output = await run("bash-huge", {command: "seq 1 500000"});
    const fields = informationFields(output);
    assert.equal(fields.truncated, "true");
    assert.ok(Number(fields.returned_bytes) < 8192 + 512);
    assert.ok(Number(fields.omitted_bytes) > 1024 * 1024);
    assert.match(output, /500000\n+--- END CONTENT/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("times out a long command and kills its process group", {skip: bashSkip}, async () => {
  const {sessionDir, run} = await openBashSession({timeout_ms: 300, kill_grace_ms: 200});
  try {
    const marker = join(sessionDir, "marker");
    const started = Date.now();
    const output = await run("bash-timeout", {command: "sleep 30; touch marker"});
    const elapsed = Date.now() - started;
    assert.equal(firstLine(output).status, "error");
    assert.match(output, /"summary":"Command timed out after 300 ms"/);
    const fields = informationFields(output);
    assert.equal(fields.error_code, "timeout");
    assert.equal(fields.timed_out, "true");
    assert.equal(fields.timeout_ms, "300");
    assert.equal(fields.truncation_reason, "timeout");
    assert.match(fields.suggestion ?? "", /timeout_ms/);
    assert.ok(elapsed < 5000, `timeout took ${elapsed} ms`);
    await delay(500);
    assert.equal(await exists(marker), false);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("escalates to SIGKILL when the command ignores SIGTERM", {skip: bashSkip}, async () => {
  const {sessionDir, run} = await openBashSession({timeout_ms: 300, kill_grace_ms: 200});
  try {
    const startedPath = join(sessionDir, "stubborn-started");
    const finishedPath = join(sessionDir, "stubborn-finished");
    const started = Date.now();
    // The loop never reaches its second touch, so the marker pair proves the
    // shell ignored SIGTERM, kept running, and was stopped by SIGKILL only.
    const output = await run("bash-stubborn", {
      command:
        "trap '' TERM; touch stubborn-started; while :; do sleep 0.05; done; touch stubborn-finished",
    });
    assert.equal(firstLine(output).status, "error");
    assert.match(output, /error_code: timeout/);
    assert.match(output, /signal: SIGKILL/);
    assert.ok(Date.now() - started < 5000);
    await delay(500);
    assert.equal(await exists(startedPath), true);
    assert.equal(await exists(finishedPath), false);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("cleans up background children when the command finishes", {skip: bashSkip}, async () => {
  const {sessionDir, run} = await openBashSession({kill_grace_ms: 200});
  try {
    const marker = join(sessionDir, "background-marker");
    const output = await run("bash-background", {
      command: "(sleep 1; touch background-marker) & echo started",
    });
    assert.equal(firstLine(output).status, "ok");
    assert.match(output, /started/);
    await delay(1500);
    assert.equal(await exists(marker), false);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("escalates after a completed command when a descendant ignores SIGTERM", {skip: bashSkip}, async () => {
  const {sessionDir, run} = await openBashSession({kill_grace_ms: 200});
  try {
    const marker = join(sessionDir, "term-ignoring-descendant-marker");
    const output = await run("bash-term-ignoring-descendant", {
      // The descendant closes the inherited pipes, so the leader's close event
      // cannot be used as proof that the whole process group has exited.
      command:
        "(trap '' TERM; sleep 1; touch term-ignoring-descendant-marker) </dev/null >/dev/null 2>&1 & echo started",
    });
    assert.equal(firstLine(output).status, "ok");
    assert.match(output, /started/);
    await delay(1500);
    assert.equal(await exists(marker), false);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("leaves background children alone with kill_process_group timeout_only", {skip: bashSkip}, async () => {
  const {sessionDir, run} = await openBashSession({
    kill_grace_ms: 200,
    kill_process_group: "timeout_only",
  });
  try {
    const marker = join(sessionDir, "survivor-marker");
    const output = await run("bash-survivor", {
      command: "(sleep 1; touch survivor-marker) & echo started",
    });
    assert.equal(firstLine(output).status, "ok");
    await delay(2500);
    assert.equal(await exists(marker), true);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("rides out a straggler that keeps stdout open", {skip: bashSkip}, async () => {
  const {sessionDir, run} = await openBashSession({
    kill_grace_ms: 200,
    kill_process_group: "timeout_only",
  });
  try {
    const started = Date.now();
    const output = await run("bash-straggler", {
      command: "sleep 30 & echo done",
    });
    const elapsed = Date.now() - started;
    assert.equal(firstLine(output).status, "ok");
    assert.match(output, /done/);
    // Only the drain window may hold the call after the leader exited.
    assert.ok(elapsed < 3000, `straggler waited ${elapsed} ms`);
    assert.doesNotMatch(output, /left_running/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("leaves no process behind after a timeout", {skip: bashSkip}, async () => {
  const {sessionDir, run} = await openBashSession({timeout_ms: 800, kill_grace_ms: 200});
  try {
    const output = await run("bash-timeout-tree", {
      command: "sleep 300 & echo child=$!; wait",
    });
    assert.equal(firstLine(output).status, "error");
    assert.match(output, /error_code: timeout/);
    assert.doesNotMatch(output, /left_running/);
    const childPid = /child=(\d+)/.exec(output)?.[1];
    assert.ok(childPid, "the background child pid must be reported");
    const check = await run("bash-timeout-tree-check", {
      command: `kill -0 ${childPid} 2>/dev/null && echo alive || echo gone`,
    });
    assert.match(check, /gone/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("bounds the whole timeout ladder for a command that ignores SIGTERM", {skip: bashSkip}, async () => {
  // The kill deadline is the last resort after SIGKILL; a call must never wait
  // for a process that does not react to it.
  const {sessionDir, run} = await openBashSession({timeout_ms: 300, kill_grace_ms: 200});
  try {
    const started = Date.now();
    const output = await run("bash-ladder-bound", {
      command: "trap '' TERM; while :; do sleep 0.05; done",
    });
    const elapsed = Date.now() - started;
    assert.match(output, /error_code: timeout/);
    assert.match(output, /signal: SIGKILL/);
    assert.equal(
      elapsed < 300 + 200 + 500 + 2000 + 2000,
      true,
      `timeout + grace + drain + kill deadline must bound the call, took ${elapsed} ms`,
    );
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("runs in the requested working directory", {skip: bashSkip}, async () => {
  const {sessionDir, run} = await openBashSession();
  try {
    await mkdir(join(sessionDir, "src"), {recursive: true});
    const workspace = await realpath(sessionDir);

    const root = await run("bash-pwd-root", {command: "pwd"});
    assert.equal(firstLine(root).status, "ok");
    assert.equal(informationFields(root).cwd, ".");
    assert.match(root, new RegExp(workspace.replace(/[/\\]/g, "\\$&")));

    const nested = await run("bash-pwd-src", {command: "pwd", cwd: "src"});
    assert.equal(informationFields(nested).cwd, "src");
    assert.match(nested, /\/src\n+--- END CONTENT/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("validates the working directory against the path policy", {skip: bashSkip}, async () => {
  const sessionDir = await makeBashToolSession();
  await refreshBashSession(sessionDir, {
    paths: [{action: "deny", path: join(sessionDir, "secret")}],
  });
  const catalog = await loadToolCatalog(await new SessionConfigLoader(sessionDir).load());
  const run: BashRunner = async (callId, args) =>
    outputText(
      (
        await catalog.executeFunctionCall(
          {
            type: "function_call",
            call_id: callId,
            name: "bash",
            arguments: JSON.stringify(args),
          },
          {workspaceRoot: sessionDir, sessionDir},
        )
      ).output,
    );
  try {
    await writeFile(join(sessionDir, "plain.txt"), "plain\n", "utf8");
    await mkdir(join(sessionDir, "secret"), {recursive: true});

    const missing = await run("bash-cwd-missing", {command: "pwd", cwd: "nope"});
    assert.equal(firstLine(missing).status, "not_found");
    assert.match(missing, /error_code: not_found/);

    const file = await run("bash-cwd-file", {command: "pwd", cwd: "plain.txt"});
    assert.equal(firstLine(file).status, "invalid_input");
    assert.match(file, /error_code: not_a_directory/);

    const outside = await run("bash-cwd-outside", {command: "pwd", cwd: "../outside"});
    assert.equal(firstLine(outside).status, "forbidden");
    assert.match(outside, /error_code: path_outside_workspace/);

    const denied = await run("bash-cwd-denied", {command: "pwd", cwd: "secret"});
    assert.equal(firstLine(denied).status, "forbidden");
    assert.match(denied, /error_code: path_denied/);

    const empty = await run("bash-empty", {command: "   "});
    assert.equal(firstLine(empty).status, "invalid_input");
    assert.match(empty, /error_code: validation_failed/);
    assert.match(empty, /example_operation/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("keeps the harness credential and denied variables out of the environment", {skip: bashSkip}, async () => {
  const {sessionDir, run} = await openBashSession(
    {
      env: {HARNESS_ADDED: "added-value", HARNESS_TEST_SECRET: "must-not-return"},
      env_deny: ["HARNESS_OTHER_SECRET"],
    },
    {api_key_env: "HARNESS_TEST_SECRET"},
  );
  const previous = {
    credential: process.env.HARNESS_TEST_SECRET,
    other: process.env.HARNESS_OTHER_SECRET,
    unlisted: process.env.HARNESS_UNLISTED_SECRET,
  };
  process.env.HARNESS_TEST_SECRET = "hunter2-secret";
  process.env.HARNESS_OTHER_SECRET = "other-secret";
  process.env.HARNESS_UNLISTED_SECRET = "unlisted-secret";
  try {
    const output = await run("bash-env", {command: "env"});
    assert.equal(firstLine(output).status, "ok");
    assert.doesNotMatch(output, /hunter2-secret/);
    assert.doesNotMatch(output, /HARNESS_TEST_SECRET/);
    assert.doesNotMatch(output, /other-secret/);
    assert.doesNotMatch(output, /HARNESS_OTHER_SECRET/);
    assert.doesNotMatch(output, /unlisted-secret/);
    assert.doesNotMatch(output, /HARNESS_UNLISTED_SECRET/);
    assert.doesNotMatch(output, /must-not-return/);
    assert.match(output, /HARNESS_ADDED=added-value/);
    assert.match(output, /NO_COLOR=1/);
    assert.match(output, /PATH=/);
  } finally {
    if (previous.credential === undefined) {
      delete process.env.HARNESS_TEST_SECRET;
    } else {
      process.env.HARNESS_TEST_SECRET = previous.credential;
    }
    if (previous.other === undefined) {
      delete process.env.HARNESS_OTHER_SECRET;
    } else {
      process.env.HARNESS_OTHER_SECRET = previous.other;
    }
    if (previous.unlisted === undefined) {
      delete process.env.HARNESS_UNLISTED_SECRET;
    } else {
      process.env.HARNESS_UNLISTED_SECRET = previous.unlisted;
    }
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("loads bash environment files relative to the session and applies overrides and denials", {skip: bashSkip}, async () => {
  const sessionDir = await makeBashToolSession({
    env_file: "command.env",
    env: {OVERRIDDEN: "inline"},
    env_deny: ["DENIED"],
  });
  try {
    await writeFile(
      join(sessionDir, "command.env"),
      [
        "# command-specific configuration",
        "FILE_VALUE=from-file",
        "OVERRIDDEN=from-file",
        "QUOTED=\"line one\\nline two # literal\"",
        "export DENIED=from-file",
        "",
      ].join("\n"),
    );
    const config = await new SessionConfigLoader(sessionDir).load();
    assert.equal(config.settings.tools.native.bash?.env_file, join(sessionDir, "command.env"));
    const catalog = await loadToolCatalog(config);
    const output = outputText(
      (
        await catalog.executeFunctionCall(
          {
            type: "function_call",
            call_id: "bash-env-file",
            name: "bash",
            arguments: JSON.stringify({
              command: "printf '%s|%s|%s|%s' \"$FILE_VALUE\" \"$OVERRIDDEN\" \"$QUOTED\" \"${DENIED-unset}\"",
            }),
          },
          {workspaceRoot: sessionDir, sessionDir},
        )
      ).output,
    );
    assert.equal(firstLine(output).status, "ok");
    assert.match(output, /from-file\|inline\|line one\nline two # literal\|unset/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("loads an absolute bash env_file and rejects malformed files", {skip: bashSkip}, async () => {
  const sessionDir = await makeBashToolSession();
  try {
    const envFile = join(sessionDir, "absolute-command.env");
    await writeFile(envFile, "ABSOLUTE_VALUE=available\n");
    await refreshBashSession(sessionDir, {env_file: envFile});
    const config = await new SessionConfigLoader(sessionDir).load();
    assert.equal(config.settings.tools.native.bash?.env_file, envFile);
    const catalog = await loadToolCatalog(config);
    const output = outputText(
      (
        await catalog.executeFunctionCall(
          {
            type: "function_call",
            call_id: "bash-absolute-env-file",
            name: "bash",
            arguments: JSON.stringify({command: "printf '%s' \"$ABSOLUTE_VALUE\""}),
          },
          {workspaceRoot: sessionDir, sessionDir},
        )
      ).output,
    );
    assert.match(output, /available/);

    await writeFile(envFile, "NOT VALID\n");
    await assert.rejects(
      () => new SessionConfigLoader(sessionDir).load(),
      /absolute-command\.env:1: expected NAME=value/,
    );
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("flags output that is not valid text", {skip: bashSkip}, async () => {
  const {sessionDir, run} = await openBashSession();
  try {
    await writeFile(
      join(sessionDir, "binary.bin"),
      Buffer.from([0x00, 0x01, 0x02, 0xff, 0x41, 0x0a]),
    );
    const output = await run("bash-binary", {command: "cat binary.bin"});
    assert.equal(firstLine(output).status, "ok");
    assert.equal(informationFields(output).binary_output, "true");
    assert.match(output, /A/);
    assert.equal(output.includes("\u0000"), false);
    assert.equal(output.includes("\uFFFD"), true);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("reports an unusable shell with a suggestion", {skip: bashSkip}, async () => {
  const {sessionDir, run} = await openBashSession({shell: "/nonexistent/bash-for-tests"});
  try {
    const output = await run("bash-no-shell", {command: "echo hello"});
    assert.equal(firstLine(output).status, "error");
    assert.match(output, /error_code: shell_unavailable/);
    assert.match(output, /suggestion: install the shell or point tools\.native\.bash\.shell/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("supports a login shell", {skip: bashSkip}, async () => {
  const {sessionDir, run} = await openBashSession({login_shell: true});
  try {
    const output = await run("bash-login", {command: "echo login-ok"});
    assert.equal(firstLine(output).status, "ok");
    assert.match(output, /login-ok/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("clamps the per-call timeout to the configured range", () => {
  const settings = {
    shell: "/bin/bash",
    login_shell: false,
    timeout_ms: 60_000,
    max_timeout_ms: 120_000,
    kill_grace_ms: 2000,
    kill_process_group: "always" as const,
    max_bytes: 1024,
    stdout: {head_bytes: 512, tail_bytes: 512},
    stderr: {head_bytes: 0, tail_bytes: 0},
    spill: {
      enabled: false,
      keep: "truncated_only" as const,
      cap_bytes: 1024,
      retention_days: 0,
      stdout_full: "{cwd}/x-{call_id}.{stream}.log",
      stderr_full: "{cwd}/x-{call_id}.{stream}.log",
      allow_unreadable: true,
    },
    max_line_chars: 100,
    strip_ansi: true,
    env: {},
    env_deny: [],
    paths: [],
  };
  assert.equal(effectiveTimeoutMs(undefined, settings), 60_000);
  assert.equal(effectiveTimeoutMs(1000, settings), 1000);
  assert.equal(effectiveTimeoutMs(1, settings), 1000);
  assert.equal(effectiveTimeoutMs(5_000_000, settings), 120_000);
});

test("applies the clamped timeout to a real call", {skip: bashSkip}, async () => {
  const {sessionDir, run} = await openBashSession({
    timeout_ms: 60_000,
    max_timeout_ms: 60_000,
    kill_grace_ms: 200,
  });
  try {
    const started = Date.now();
    const output = await run("bash-clamped", {command: "sleep 30", timeout_ms: 1});
    assert.equal(firstLine(output).status, "error");
    assert.match(output, /error_code: timeout/);
    assert.match(output, /timeout_ms: 1000/);
    assert.ok(Date.now() - started < 5000);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("validates bash settings and inherits write path rules", async () => {
  const sessionDir = await makeBashToolSession();
  try {
    const writeRules = [{action: "deny", path: "secret"}];
    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({
        cwd: sessionDir,
        tool_definition_paths: [join(sessionDir, "bash.json")],
        tools: {native: {write: {paths: writeRules}, bash: {timeout_ms: 30000}}},
      }),
    );
    const config = await new SessionConfigLoader(sessionDir).load();
    const bash = config.settings.tools.native.bash!;
    assert.equal(bash.shell, "/bin/bash");
    assert.equal(bash.login_shell, false);
    assert.equal(bash.timeout_ms, 30_000);
    assert.equal(bash.max_timeout_ms, 600_000);
    assert.equal(bash.kill_grace_ms, 2000);
    assert.equal(bash.kill_process_group, "always");
    assert.equal(bash.max_bytes, 65536);
    assert.deepEqual(bash.stdout, {head_bytes: 16 * 1024, tail_bytes: 32 * 1024});
    assert.deepEqual(bash.stderr, {head_bytes: 4 * 1024, tail_bytes: 12 * 1024});
    assert.equal(bash.spill.enabled, true);
    assert.equal(bash.spill.keep, "truncated_only");
    assert.equal(bash.spill.cap_bytes, 64 * 1024 * 1024);
    assert.equal(bash.spill.retention_days, 14);
    assert.equal(bash.spill.stdout_full, "{cwd}/.bash-output/bash-{call_id}.{stream}.log");
    assert.equal(bash.spill.allow_unreadable, false);
    assert.equal(bash.max_line_chars, 2000);
    assert.equal(bash.strip_ansi, true);
    assert.equal(bash.env.NO_COLOR, "1");
    assert.equal(bash.env.GIT_TERMINAL_PROMPT, "0");
    assert.deepEqual(bash.env_deny, []);
    assert.deepEqual(bash.paths, config.settings.tools.native.write?.paths);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }

  const invalid = await makeBashToolSession();
  try {
    for (const settings of [
      {shell: "   "},
      {login_shell: "yes"},
      {timeout_ms: 0},
      {timeout_ms: 5000, max_timeout_ms: 1000},
      {kill_grace_ms: -1},
      {kill_process_group: "sometimes"},
      {max_bytes: 0},
      // Stream budgets must fit inside the ceiling.
      {max_bytes: 1024, stdout: {head_bytes: 2048, tail_bytes: 2048}},
      {stdout: {head_bytes: -1}},
      // The spill template needs a call id and known placeholders.
      {spill: {stdout_full: "fixed.log"}},
      {spill: {stdout_full: "{cwd}/{nope}-{call_id}.log"}},
      // Both streams cannot share one file.
      {
        spill: {
          stdout_full: "{cwd}/same-{call_id}.log",
          stderr_full: "{cwd}/same-{call_id}.log",
        },
      },
      {spill: {keep: "sometimes"}},
      {spill: {cap_bytes: 0}},
      // A location outside the session cwd is unreachable for read and grep.
      {
        spill: {
          stdout_full: "/tmp/agent-harness-spill-{call_id}.{stream}.log",
          stderr_full: "/tmp/agent-harness-spill-{call_id}.{stream}.log",
        },
      },
      {max_line_chars: 5},
      {strip_ansi: "yes"},
      {env: {HARNESS_BAD: 1}},
      {env_deny: ["1BAD-NAME"]},
    ]) {
      await refreshBashSession(invalid, settings as NativeBashSessionOptions);
      await assert.rejects(
        new SessionConfigLoader(invalid).load(),
        /tools\.native\.bash\./,
      );
    }
  } finally {
    await rm(invalid, {recursive: true, force: true});
  }

  // Removed keys are refused with their replacement instead of being ignored;
  // the session helper only forwards known options, so the document is built here.
  const removed = await makeBashToolSession();
  try {
    for (const [key, value] of [["capture_bytes", 1024], ["head_ratio", 0.5]] as const) {
      await writeFile(
        join(removed, "setting.json"),
        JSON.stringify({
          cwd: removed,
          tool_definition_paths: [join(removed, "bash.json")],
          tools: {native: {bash: {[key]: value}}},
        }),
      );
      await assert.rejects(
        new SessionConfigLoader(removed).load(),
        /tools\.native\.bash\./,
      );
    }
  } finally {
    await rm(removed, {recursive: true, force: true});
  }
});

test("requires the bash definition when bash is configured", async () => {
  const sessionDir = await makeBashToolSession();
  try {
    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({
        cwd: sessionDir,
        tool_definition_paths: [],
        tools: {native: {bash: {}}},
      }),
    );
    const config = await new SessionConfigLoader(sessionDir).load();
    await assert.rejects(
      loadToolCatalog(config),
      /tools\.native\.bash is configured but native\.bash is missing from tool_definition_paths/,
    );
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("normalizes near-miss argument spellings", {skip: bashSkip}, async () => {
  const {sessionDir, run} = await openBashSession();
  try {
    await mkdir(join(sessionDir, "src"), {recursive: true});
    const output = await run("bash-aliases", {cmd: "  pwd  ", workdir: "src"});
    assert.equal(firstLine(output).status, "ok");
    assert.equal(informationFields(output).cwd, "src");
    assert.match(output, /\/src\n+--- END CONTENT/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("strips control characters and cuts long lines deterministically", () => {
  assert.equal(stripControlCharacters("plain text"), "plain text");
  assert.equal(stripControlCharacters("\u001b[31mred\u001b[0m"), "red");
  assert.equal(stripControlCharacters("a\u0007b\u0000c"), "abc");
  assert.equal(stripControlCharacters("progress 50%\rprogress 100%\n"), "progress 50%\nprogress 100%\n");
  assert.equal(stripControlCharacters("keep\ttab\nnewline"), "keep\ttab\nnewline");
  assert.equal(stripControlCharacters("emoji \u{1f600} stays"), "emoji \u{1f600} stays");
  assert.equal(stripControlCharacters("\u001b]0;title\u0007after"), "after");
  assert.equal(cutLongLines("abcdef", 3), "abc [... truncated line]");
  assert.equal(cutLongLines("ab\ncd", 3), "ab\ncd");
});

test("never expands a rendered stream beyond its byte window", () => {
  const collector = new OutputCollector({
    stdout: {headBytes: 4, tailBytes: 0},
    stderr: {headBytes: 0, tailBytes: 0},
    maxLineChars: 1,
    stripAnsi: true,
  });
  collector.push("stdout", Buffer.from("abcd"));
  collector.end();
  assert.ok(collector.render().returnedBytes <= 4);
});

test("signals and drains spill backpressure", async () => {
  const directory = await mkdtemp(join(tmpdir(), "agent-harness-spill-backpressure-"));
  try {
    const writer = new SpillWriter({path: join(directory, "output.log"), capBytes: 128 * 1024});
    const drained = writer.write(Buffer.alloc(64 * 1024, "x"));
    assert.ok(drained instanceof Promise);
    await drained;
    const outcome = await writer.close();
    assert.equal(outcome.writtenBytes, 64 * 1024);
    assert.equal((await readFile(outcome.path)).length, 64 * 1024);
  } finally {
    await rm(directory, {recursive: true, force: true});
  }
});

test("labels each stream when both wrote", {skip: bashSkip}, async () => {
  const {sessionDir, run} = await openBashSession({spill: {enabled: false}});
  try {
    const output = await run("bash-labeled", {
      command: "printf 'out\\n'; printf 'err\\n' 1>&2",
    });
    assert.equal(firstLine(output).status, "ok");
    assert.equal(informationFields(output).content_stream, "mixed");
    assert.match(output, /\[stdout\]\nout\n\[stderr\]\nerr\n/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("keeps stderr when stdout floods", {skip: bashSkip}, async () => {
  const {sessionDir, run} = await openBashSession({
    max_bytes: 8192,
    stdout: {head_bytes: 1024, tail_bytes: 1024},
    stderr: {head_bytes: 1024, tail_bytes: 1024},
  });
  try {
    const output = await run("bash-flood-stderr", {
      command: "printf 'important-error\\n' 1>&2; seq 1 200000",
    });
    const fields = informationFields(output);
    // Each stream owns its windows, so the flood cannot evict the error.
    assert.equal(fields.content_stream, "mixed");
    assert.equal(fields.stderr_omitted_bytes, undefined);
    assert.equal(fields.stderr_shown_bytes, "16");
    assert.match(output, /\[stderr\]\nimportant-error/);
    assert.match(output, /200000/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("spills a truncated stream to a file the model can read", {skip: bashSkip}, async () => {
  const {sessionDir, run} = await openBashSession({
    max_bytes: 2048,
    stdout: {head_bytes: 512, tail_bytes: 512},
    stderr: {head_bytes: 512, tail_bytes: 512},
  });
  try {
    const output = await run("bash-spill", {command: "seq 1 5000"});
    const fields = informationFields(output);
    assert.equal(fields.truncated, "true");
    assert.equal(fields.content_stream, "stdout");
    assert.ok(fields.stdout_full, "the complete stdout must be reported");
    const written = await readFile(join(sessionDir, fields.stdout_full!), "utf8");
    // The file holds every byte, including the middle the content dropped.
    assert.equal(written.split("\n").length - 1, 5000);
    assert.match(written, /^1\n2\n3\n/);
    assert.match(written, /5000\n$/);
    assert.ok(Number(fields.stdout_full_bytes) > 2048);
    assert.match(fields.hint ?? "", /read or grep/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("removes the spill file when the model saw the whole stream", {skip: bashSkip}, async () => {
  const {sessionDir, run} = await openBashSession();
  try {
    const output = await run("bash-spill-unused", {command: "printf 'small\\n'"});
    assert.equal(firstLine(output).status, "ok");
    assert.equal(informationFields(output).stdout_full, undefined);
    const entries = await readdir(join(sessionDir, ".bash-output"));
    assert.deepEqual(entries, []);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("keeps every call's output when keep is always", {skip: bashSkip}, async () => {
  const {sessionDir, run} = await openBashSession({spill: {keep: "always"}});
  try {
    const output = await run("bash-spill-always", {command: "printf 'kept\\n'"});
    const fields = informationFields(output);
    assert.equal(fields.stdout_full, ".bash-output/bash-bash-spill-always.stdout.log");
    const written = await readFile(join(sessionDir, fields.stdout_full!), "utf8");
    assert.equal(written, "kept\n");
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("caps a spill file and says so", {skip: bashSkip}, async () => {
  const {sessionDir, run} = await openBashSession({
    max_bytes: 2048,
    stdout: {head_bytes: 512, tail_bytes: 512},
    stderr: {head_bytes: 512, tail_bytes: 512},
    spill: {cap_bytes: 512},
  });
  try {
    const output = await run("bash-spill-cap", {command: "seq 1 5000"});
    const fields = informationFields(output);
    assert.equal(fields.stdout_full_capped, "true");
    assert.ok(Number(fields.stdout_full_bytes) <= 512);
    const written = await readFile(join(sessionDir, fields.stdout_full!), "utf8");
    assert.ok(Buffer.byteLength(written, "utf8") <= 512);
    assert.match(written, /^1\n/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("reports a spill failure without failing the command", {skip: bashSkip}, async () => {
  const {sessionDir, run} = await openBashSession({
    max_bytes: 2048,
    stdout: {head_bytes: 512, tail_bytes: 512},
    stderr: {head_bytes: 512, tail_bytes: 512},
    spill: {stdout_full: "{cwd}/blocked/out-{call_id}.{stream}.log"},
  });
  try {
    // A file where the directory should be makes the spill impossible.
    await writeFile(join(sessionDir, "blocked"), "not a directory\n");
    const output = await run("bash-spill-failed", {command: "seq 1 5000"});
    assert.equal(firstLine(output).status, "ok");
    const fields = informationFields(output);
    assert.equal(fields.truncated, "true");
    assert.ok(fields.spill_error, "the failure must be reported");
    assert.match(String(fields.spill_error), /stdout/);
    assert.equal(fields.stdout_full, undefined);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("prunes expired spill files and leaves other files alone", {skip: bashSkip}, async () => {
  const {sessionDir, run} = await openBashSession({spill: {retention_days: 1}});
  try {
    const directory = join(sessionDir, ".bash-output");
    await mkdir(directory, {recursive: true});
    const expired = join(directory, "bash-old-call.stdout.log");
    const unrelated = join(directory, "notes.txt");
    await writeFile(expired, "old\n");
    await writeFile(unrelated, "keep me\n");
    const threeDaysAgo = new Date(Date.now() - 3 * 24 * 60 * 60 * 1000);
    await utimes(expired, threeDaysAgo, threeDaysAgo);

    await run("bash-prune", {command: "printf 'hello\\n'"});
    assert.equal(await exists(expired), false);
    assert.equal(await exists(unrelated), true);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

// The collector is exported for reuse; keep its accounting honest without a
// child process by feeding synthetic chunks.
test("collects head and tail windows per stream without duplication", () => {
  const limits = {
    stdout: {headBytes: 10, tailBytes: 6},
    stderr: {headBytes: 4, tailBytes: 4},
    maxLineChars: 100,
    stripAnsi: true,
  };

  const small = new OutputCollector(limits);
  small.push("stdout", Buffer.from("abcdefghij", "utf8"));
  small.end();
  const kept = small.render();
  assert.equal(kept.text, "abcdefghij");
  assert.equal(kept.truncated, false);
  assert.equal(kept.omittedBytes, 0);
  assert.equal(kept.stdout.producedBytes, 10);
  assert.equal(kept.stdout.shownBytes, 10);
  assert.equal(kept.returnedBytes, 10);

  const windowed = new OutputCollector(limits);
  windowed.push("stdout", Buffer.from("abcdefghij", "utf8"));
  windowed.push("stderr", Buffer.from("klmnopqrst", "utf8"));
  windowed.push("stdout", Buffer.from("uvwxyz", "utf8"));
  windowed.end();
  const bounded = windowed.render();
  // A chatty stdout cannot evict stderr: each stream owns its windows.
  assert.equal(bounded.stdout.producedBytes, 16);
  assert.equal(bounded.stdout.truncated, false);
  assert.equal(bounded.stderr.producedBytes, 10);
  assert.equal(bounded.stderr.shownBytes, 8);
  assert.equal(bounded.stderr.omittedBytes, 2);
  assert.equal(bounded.truncated, true);
  assert.equal(bounded.omittedBytes, 2);
  // Sections are labeled because both streams wrote.
  assert.match(bounded.text, /^\[stdout\]\nabcdefghijuvwxyz/);
  assert.match(bounded.text, /\[stderr\]\nklmn\n\[\.\.\. 2 bytes omitted \.\.\.\]\nqrst$/);
});
