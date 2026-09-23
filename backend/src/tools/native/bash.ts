/**
 * @file bash.ts
 * @input Raw `bash` arguments plus the session settings, workspace, and policy
 * @output Exports the `bash` definition and executeNativeBash()
 * @position Native tool that runs one shell command per call
 *
 * The tool is the fallback for work no dedicated tool covers (builds, tests,
 * package managers, git, pipelines) and deliberately has no shell state: every
 * call starts a fresh shell in a resolved working directory, because a carried
 * over `cd` or `export` would make results depend on earlier calls.
 *
 * It is not a sandbox: the path policy only decides which directory a command
 * starts in, and the command itself runs with the backend user's rights. What
 * the tool does own is hygiene — no inherited harness credential, no leaked
 * process tree, no hang waiting for a prompt, and output bounded in bytes.
 */

import {stat, unlink} from "node:fs/promises";
import {basename, dirname, join} from "node:path";

import {
  DEFAULT_NATIVE_BASH_ENV,
  DEFAULT_NATIVE_BASH_SPILL_FILENAME,
  DEFAULT_NATIVE_BASH_SPILL_PATH,
  DEFAULT_NATIVE_BASH_SPILL_STORAGE_DIR,
  MIN_NATIVE_BASH_CALL_TIMEOUT_MS,
  type NativeBashToolSettings,
} from "../../config.js";
import type {JsonRecord} from "../../persistence.js";
import {executionError, renderToolResult} from "../output.js";
import type {ToolDefinition, ToolExecutionContext} from "../types.js";
import {type ChildProcessResult, type ChildStream, runChildProcess, type SpillTarget, type StreamLimits} from "./child.js";
import {displayPath, resolveExistingTarget} from "./path.js";
import {
  encodeSpillCallId,
  prepareSpillDir,
  pruneSpillFiles,
  resolveSpillPath,
  spillPrunePattern,
  type SpillVars,
} from "./spill.js";

export const NATIVE_BASH_ID = "native.bash";

export const nativeBashDefinition: ToolDefinition = {
  id: NATIVE_BASH_ID,
  source: "native",
  name: "bash",
  description: "Run a bounded shell command. Use file tools for file operations.",
  inputSchema: {
    type: "object",
    properties: {
      command: {
        type: "string",
        minLength: 1,
        description: "Shell command line, executed with the configured shell.",
      },
      cwd: {
        type: "string",
        minLength: 1,
        description:
          "Directory to run in, relative to the workspace root or absolute. Defaults to the workspace root.",
      },
      timeout_ms: {
        type: "integer",
        description:
          "Wall-clock budget for this call in milliseconds; clamped to the configured range.",
      },
    },
    required: ["command"],
    additionalProperties: false,
  },
};

export interface NativeBashExecutionContext extends ToolExecutionContext {
  policy: NativeBashToolSettings;
  /** Environment variable holding the harness credential; always removed. */
  apiKeyEnv: string;
  environment?: NodeJS.ProcessEnv;
}

function stringField(value: unknown): string | undefined {
  return typeof value === "string" && value.length > 0 ? value : undefined;
}

function integerField(value: unknown): number | undefined {
  return typeof value === "number" && Number.isSafeInteger(value) ? value : undefined;
}

/** Per-call override, clamped so a typo cannot kill a command instantly. */
export function effectiveTimeoutMs(
  requested: number | undefined,
  settings: NativeBashToolSettings,
): number {
  if (requested === undefined) {
    return settings.timeout_ms;
  }
  const floor = Math.min(MIN_NATIVE_BASH_CALL_TIMEOUT_MS, settings.max_timeout_ms);
  return Math.min(Math.max(requested, floor), settings.max_timeout_ms);
}

/**
 * The only inherited variables a command needs to locate programs and behave
 * normally. Credentials and service configuration must be added deliberately
 * through `tools.native.bash.env`, never inherited from the backend process.
 */
const COMMAND_ENV_ALLOWLIST = [
  "PATH",
  "HOME",
  "LANG",
  "LC_ALL",
  "LC_CTYPE",
  "TMPDIR",
  "TMP",
  "TEMP",
  "SYSTEMROOT",
  "WINDIR",
  "ComSpec",
  "PATHEXT",
] as const;

/**
 * Environment for the child: a small runtime allowlist plus configured values.
 * Denials are applied last, so neither defaults nor configured additions can
 * accidentally reintroduce the harness credential.
 */
export function buildCommandEnvironment(
  base: NodeJS.ProcessEnv,
  settings: NativeBashToolSettings,
  apiKeyEnv: string,
): NodeJS.ProcessEnv {
  const denied = new Set([apiKeyEnv, ...settings.env_deny]);
  const env: NodeJS.ProcessEnv = {};
  for (const name of COMMAND_ENV_ALLOWLIST) {
    const value = base[name];
    if (value !== undefined && !denied.has(name)) {
      env[name] = value;
    }
  }
  for (const [name, value] of Object.entries({...DEFAULT_NATIVE_BASH_ENV, ...settings.env})) {
    if (!denied.has(name)) {
      env[name] = value;
    }
  }
  return env;
}

function toStreamLimits(budget: {head_bytes: number; tail_bytes: number}): StreamLimits {
  return {headBytes: budget.head_bytes, tailBytes: budget.tail_bytes};
}

/** Per-call spill plan: where complete streams go if they need to be kept. */
export interface SpillPlan {
  targets: {stdout?: SpillTarget; stderr?: SpillTarget};
  vars: SpillVars;
}

/**
 * Resolves both per-stream output files from storage_dir or legacy templates,
 * creates their directories, and prunes expired files. Nothing here can fail the
 * call: a location that cannot be prepared surfaces later as spill_error on the result.
 */
export async function planSpill(
  policy: NativeBashToolSettings,
  workspaceRoot: string,
  sessionDir: string,
  callId: string,
): Promise<SpillPlan | undefined> {
  if (!policy.spill.enabled) {
    return undefined;
  }
  const vars: SpillVars = {
    cwd: workspaceRoot,
    session_dir: sessionDir,
    session_name: basename(sessionDir),
    call_id: callId,
    stream: "stdout",
  };
  const useLegacyTemplates = policy.spill.stdout_full !== undefined || policy.spill.stderr_full !== undefined;
  const generatedVars = {...vars, call_id: encodeSpillCallId(callId)};
  const storageDir = policy.spill.storage_dir ?? join(workspaceRoot, DEFAULT_NATIVE_BASH_SPILL_STORAGE_DIR);
  const paths = {} as Record<ChildStream, string>;
  for (const stream of ["stdout", "stderr"] as ChildStream[]) {
    const template = useLegacyTemplates
      ? policy.spill[`${stream}_full`] ?? DEFAULT_NATIVE_BASH_SPILL_PATH
      : DEFAULT_NATIVE_BASH_SPILL_FILENAME;
    const streamVars = {...(useLegacyTemplates ? vars : generatedVars), stream};
    const filePath = resolveSpillPath(template, streamVars);
    paths[stream] = useLegacyTemplates ? filePath : join(storageDir, filePath);
  }
  await Promise.all(Object.values(paths).map((path) => prepareSpillDir(path).catch(() => undefined)));
  await Promise.all(
    (["stdout", "stderr"] as ChildStream[]).map((stream) => {
      const template = useLegacyTemplates
        ? policy.spill[`${stream}_full`] ?? DEFAULT_NATIVE_BASH_SPILL_PATH
        : DEFAULT_NATIVE_BASH_SPILL_FILENAME;
      const streamVars = {...(useLegacyTemplates ? vars : generatedVars), stream};
      return pruneSpillFiles({
        directory: dirname(paths[stream]),
        pattern: spillPrunePattern(template, streamVars),
        retentionDays: policy.spill.retention_days,
      }).catch(() => 0);
    }),
  );
  return {
    targets: {
      stdout: {path: paths.stdout, capBytes: policy.spill.cap_bytes},
      stderr: {path: paths.stderr, capBytes: policy.spill.cap_bytes},
    },
    vars,
  };
}

export interface KeptSpill {
  path: string;
  /** How the model addresses the file: workspace-relative or absolute. */
  display: string;
  writtenBytes: number;
  capped: boolean;
}

interface SpillDecision {
  stdout?: KeptSpill;
  stderr?: KeptSpill;
  error?: string;
  hint?: string;
}

/**
 * Applies the keep policy to the spill files. Output the model saw in full needs
 * no file unless the session keeps every spill, so it is removed right away; a
 * truncated stream keeps its file and is advertised, because the file is the only
 * place the dropped middle still exists.
 */
export async function applySpillPolicy(
  policy: NativeBashToolSettings,
  plan: SpillPlan | undefined,
  result: ChildProcessResult,
  workspaceRoot: string,
): Promise<SpillDecision> {
  if (!plan) {
    return {};
  }
  const decision: SpillDecision = {};
  const errors: string[] = [];
  for (const stream of ["stdout", "stderr"] as ChildStream[]) {
    const outcome = result.spill?.[stream];
    if (!outcome) {
      continue;
    }
    if (outcome.error !== undefined) {
      // A file that could not be written is never advertised as complete.
      errors.push(`${stream}: ${outcome.error}`);
      continue;
    }
    if (outcome.writtenBytes === 0) {
      continue;
    }
    const keep = policy.spill.keep === "always" || result[stream].truncated;
    if (!keep) {
      await unlink(outcome.path).catch(() => undefined);
      continue;
    }
    decision[stream] = {
      path: outcome.path,
      display: displayPath(workspaceRoot, outcome.path),
      writtenBytes: outcome.writtenBytes,
      capped: outcome.capped,
    };
  }
  if (errors.length > 0) {
    decision.error = errors.join("; ");
  }
  const files = [decision.stdout, decision.stderr]
    .filter((entry): entry is KeptSpill => entry !== undefined)
    .map((entry) => entry.display);
  if (files.length > 0) {
    decision.hint = `the complete output is in ${files.join(" and ")}; read or grep it instead of re-running the command`;
  }
  return decision;
}

export async function executeNativeBash(
  args: JsonRecord,
  context: NativeBashExecutionContext,
): Promise<string> {
  const {callId, workspaceRoot, policy, apiKeyEnv} = context;
  const command = typeof args.command === "string" ? args.command.trim() : "";
  if (command.length === 0) {
    return executionError(
      callId,
      "invalid_input",
      "invalid_command",
      "The command is empty",
    );
  }

  const requestedCwd = stringField(args.cwd) ?? ".";
  const resolved = await resolveExistingTarget(requestedCwd, workspaceRoot, policy, callId);
  if (!resolved.target) {
    return resolved.error!;
  }
  const {realPath, displayPath} = resolved.target;
  let target;
  try {
    target = await stat(realPath);
  } catch {
    return executionError(callId, "error", "cwd_unavailable", "The working directory is not readable", [
      ["path", displayPath],
    ]);
  }
  if (!target.isDirectory()) {
    return executionError(callId, "invalid_input", "not_a_directory", "The working directory is not a directory", [
      ["path", displayPath],
    ]);
  }

  const timeoutMs = effectiveTimeoutMs(integerField(args.timeout_ms), policy);
  const spillPlan = await planSpill(policy, workspaceRoot, context.sessionDir, callId);
  const result = await runChildProcess({
    file: policy.shell,
    args: policy.login_shell ? ["-l", "-c", command] : ["-c", command],
    cwd: realPath,
    env: buildCommandEnvironment(context.environment ?? process.env, policy, apiKeyEnv),
    timeoutMs,
    killGraceMs: policy.kill_grace_ms,
    killProcessGroupWhenDone: policy.kill_process_group === "always",
    signal: context.signal,
    output: {
      stdout: toStreamLimits(policy.stdout),
      stderr: toStreamLimits(policy.stderr),
      maxLineChars: policy.max_line_chars,
      stripAnsi: policy.strip_ansi,
    },
    ...(spillPlan ? {spill: spillPlan.targets} : {}),
  });

  const kept = await applySpillPolicy(policy, spillPlan, result, workspaceRoot);
  const information: Array<[string, unknown]> = [
    ["cwd", displayPath],
    // A process the deadline left behind is reported, never silently assumed gone.
    ...(result.leftRunning
      ? [["left_running", true] as [string, unknown]]
      : []),
  ];
  const content = result.text.length > 0
    ? {text: result.text, encoding: "utf8" as const}
    : undefined;
  // A command that never finished produced incomplete output even when the
  // byte budget had room, so a timeout always counts as truncation.
  const truncated = result.truncated || result.timedOut;
  const contentStream = result.stdout.text.length > 0 && result.stderr.text.length > 0
    ? "mixed"
    : result.stderr.text.length > 0
      ? "stderr"
      : result.stdout.text.length > 0
        ? "stdout"
        : undefined;
  const measured: Array<[string, unknown]> = [
    ...(result.exitCode !== null && result.exitCode !== 0
      ? [["exit_code", result.exitCode] as [string, unknown]]
      : []),
    ...(result.signal ? [["signal", result.signal] as [string, unknown]] : []),
    ...(result.timedOut ? [["timed_out", true] as [string, unknown]] : []),
    ...(contentStream && truncated ? [["content_stream", contentStream] as [string, unknown]] : []),
    ...(truncated ? [["truncated", true] as [string, unknown]] : []),
    ...(truncated
      ? [["truncation_reason", result.timedOut ? "timeout" : "max_bytes"] as [string, unknown]]
      : []),
    ...(result.omittedBytes > 0
      ? [["omitted_bytes", result.omittedBytes] as [string, unknown]]
      : []),
    ...(kept.stdout ? [["stdout_full", kept.stdout.display] as [string, unknown]] : []),
    ...(kept.stdout?.capped ? [["stdout_full_capped", true] as [string, unknown]] : []),
    ...(kept.stderr ? [["stderr_full", kept.stderr.display] as [string, unknown]] : []),
    ...(kept.stderr?.capped ? [["stderr_full_capped", true] as [string, unknown]] : []),
    ...(kept.error ? [["spill_error", kept.error] as [string, unknown]] : []),
    ...(kept.hint ? [["hint", kept.hint] as [string, unknown]] : []),
    ...(result.binary ? [["binary_output", true] as [string, unknown]] : []),
  ];

  const render = (
    status: "ok" | "error",
    summary: string,
    errorFields: Array<[string, unknown]> = [],
  ): string =>
    renderToolResult({
      id: callId,
      status,
      summary,
      information:
        status === "ok"
          ? [...information, ...measured]
          : [...errorFields, ...information, ...measured],
      ...(content ? {content} : {}),
    });

  if (result.spawnError) {
    const unavailable = result.spawnError.code === "ENOENT";
    return render("error", unavailable ? "The shell is not available" : "The command could not be started", [
      ["error_type", "execution"],
      ["error_code", unavailable ? "shell_unavailable" : "spawn_failed"],
      ["shell", policy.shell],
      ...(unavailable
        ? [["suggestion", "install the shell or point tools.native.bash.shell at an executable"] as [string, unknown]]
        : [["system_error", result.spawnError.message] as [string, unknown]]),
    ]);
  }

  if (result.timedOut) {
    return render("error", result.leftRunning
      ? `Command timed out after ${timeoutMs} ms and survived SIGKILL`
      : `Command timed out after ${timeoutMs} ms`, [
      ["error_type", "execution"],
      ["error_code", "timeout"],
      ["timeout_ms", timeoutMs],
      ["suggestion", result.leftRunning
        ? "the process is blocked in the kernel and was left to the operating system; nothing of it can be reached from here"
        : "raise timeout_ms for this call or tools.native.bash.timeout_ms, or narrow the command"],
    ]);
  }

  if (result.signal) {
    return render("error", `Command was killed by ${result.signal}`, [
      ["error_type", "execution"],
      ["error_code", "signaled"],
    ]);
  }

  if (result.exitCode !== 0) {
    const notFound = result.exitCode === 126 || result.exitCode === 127;
    return render("error", `Command exited with ${result.exitCode}`, [
      ["error_type", "execution"],
      ["error_code", "nonzero_exit"],
      ...(notFound
        ? [["hint", "exit code 127 means the command was not found; 126 means it is not executable"] as [string, unknown]]
        : []),
    ]);
  }

  return render("ok", `Command completed in ${result.durationMs} ms`);
}
