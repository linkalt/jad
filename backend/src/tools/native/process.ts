import {stat} from "node:fs/promises";

import type {NativeProcessToolSettings} from "../../config.js";
import type {JsonRecord} from "../../persistence.js";
import {executionError, renderToolResult} from "../output.js";
import type {ToolDefinition, ToolExecutionContext} from "../types.js";
import {resolveExistingTarget} from "./path.js";
import {ProcessManager, type ProcessSnapshot, type ProcessState} from "./process-manager.js";

export const NATIVE_PROCESS_ID = "native.process";

export const nativeProcessDefinition: ToolDefinition = {
  id: NATIVE_PROCESS_ID,
  source: "native",
  name: "process",
  description: "Start and manage a long-running shell process. Call with command to start, id to inspect status and recent logs, or no arguments to list session processes. Use action wait to wait briefly for one process and stop to terminate one managed process.",
  inputSchema: {
    type: "object",
    properties: {
      action: {type: "string", enum: ["start", "status", "list", "wait", "stop"], description: "Usually inferred from command or id."},
      command: {type: "string", minLength: 1, description: "Shell command to start."},
      cwd: {type: "string", minLength: 1, description: "Working directory for start. Defaults to the workspace."},
      id: {type: "string", minLength: 1, description: "Id returned by a prior start."},
      filter: {type: "string", enum: ["running", "exited", "stopped", "failed", "lost"], description: "State filter for list."},
      timeout_ms: {type: "integer", minimum: 1, description: "Maximum time wait blocks; clamped to session policy."},
    },
    required: [],
    additionalProperties: false,
  },
};

export interface NativeProcessExecutionContext extends ToolExecutionContext {
  policy: NativeProcessToolSettings;
  manager: ProcessManager;
  apiKeyEnv: string;
  environment?: NodeJS.ProcessEnv;
}

type ProcessAction = "start" | "status" | "list" | "wait" | "stop";

function text(value: unknown): string | undefined {
  return typeof value === "string" && value.trim().length > 0 ? value.trim() : undefined;
}

function actionFor(args: JsonRecord): ProcessAction | undefined {
  const action = text(args.action) as ProcessAction | undefined;
  if (action) return action;
  if (text(args.command)) return "start";
  if (text(args.id)) return "status";
  return "list";
}

export function normalizeProcessArguments(args: JsonRecord): JsonRecord {
  const normalized: JsonRecord = {...args};
  const suppliedFilter = typeof args.filter === "string";
  for (const key of ["command", "cwd", "id", "filter"] as const) {
    if (typeof normalized[key] === "string") normalized[key] = normalized[key].trim();
  }
  const actionAliases: Record<string, ProcessAction> = {
    run: "start", launch: "start", state: "status", info: "status", kill: "stop", terminate: "stop",
  };
  if (typeof normalized.action === "string") {
    normalized.action = actionAliases[normalized.action.trim().toLowerCase()] ?? normalized.action.trim().toLowerCase();
  }
  if (typeof normalized.filter === "string") {
    const filter = normalized.filter.trim().toLowerCase();
    normalized.filter = filter === "all" || filter.length === 0 ? undefined : filter;
  }
  if (normalized.action === undefined && (normalized.filter !== undefined || suppliedFilter)) normalized.action = "list";
  return Object.fromEntries(Object.entries(normalized).filter(([, value]) => value !== undefined));
}

function renderSnapshot(callId: string, action: ProcessAction, snapshot: ProcessSnapshot, extra: Array<[string, unknown]> = []): string {
  const information: Array<[string, unknown]> = [
    ["action", action], ["process_id", snapshot.id], ["state", snapshot.state], ["cwd", snapshot.cwd],
    ...(snapshot.pid === undefined ? [] : [["pid", snapshot.pid] as [string, unknown]]),
    ...(snapshot.exit_code === undefined ? [] : [["exit_code", snapshot.exit_code] as [string, unknown]]),
    ...(snapshot.signal === undefined ? [] : [["signal", snapshot.signal] as [string, unknown]]),
    ...(snapshot.left_running ? [["left_running", true] as [string, unknown]] : []),
    ["stdout_produced_bytes", snapshot.output.stdout.producedBytes],
    ["stderr_produced_bytes", snapshot.output.stderr.producedBytes],
    ...(snapshot.stdout_capped ? [["stdout_log_capped", true] as [string, unknown]] : []),
    ...(snapshot.stderr_capped ? [["stderr_log_capped", true] as [string, unknown]] : []),
    ...extra,
  ];
  return renderToolResult({
    id: callId,
    status: snapshot.state === "failed" ? "error" : "ok",
    summary: `Process ${snapshot.id} is ${snapshot.state}`,
    information,
    ...(snapshot.output.text.length > 0 ? {content: {text: snapshot.output.text, encoding: "utf8"}} : {}),
  });
}

export async function executeNativeProcess(args: JsonRecord, context: NativeProcessExecutionContext): Promise<string> {
  const {callId, workspaceRoot, policy, manager} = context;
  const action = actionFor(args);
  if (!action) return executionError(callId, "invalid_input", "invalid_action", "Process action is invalid");
  const command = text(args.command);
  const id = text(args.id);
  const filter = text(args.filter) as ProcessState | undefined;
  if (action === "start") {
    if (!command || id) return executionError(callId, "invalid_input", "invalid_start", "Start requires command and no process id");
    const requestedCwd = text(args.cwd) ?? ".";
    const resolved = await resolveExistingTarget(requestedCwd, workspaceRoot, policy, callId);
    if (!resolved.target) return resolved.error!;
    const target = await stat(resolved.target.realPath).catch(() => undefined);
    if (!target?.isDirectory()) return executionError(callId, "invalid_input", "not_a_directory", "The working directory is not a directory", [["path", resolved.target.displayPath]]);
    try {
      const snapshot = await manager.start({
        cwd: resolved.target.realPath,
        displayCwd: resolved.target.displayPath,
        command,
        policy,
        environment: context.environment ?? process.env,
        apiKeyEnv: context.apiKeyEnv,
      });
      return renderSnapshot(callId, action, snapshot);
    } catch (error) {
      return executionError(callId, "error", "process_start_failed", error instanceof Error ? error.message : String(error));
    }
  }
  if (action === "list") {
    if (command || id) return executionError(callId, "invalid_input", "invalid_list", "List accepts no command or process id");
    return renderToolResult({
      id: callId,
      status: "ok",
      summary: "Listed managed processes",
      information: [["action", "list"], ["processes", manager.list(filter).map(({output, ...item}) => item)]],
    });
  }
  if (!id || command || filter) {
    return executionError(callId, "invalid_input", "invalid_target", `${action} requires a process id and no incompatible fields`);
  }
  const existing = await manager.snapshotWithLogs(
    id,
    {headBytes: policy.stdout.head_bytes, tailBytes: policy.stdout.tail_bytes},
    {headBytes: policy.stderr.head_bytes, tailBytes: policy.stderr.tail_bytes},
    policy.max_line_chars,
    policy.strip_ansi,
  );
  if (!existing) return executionError(callId, "not_found", "process_not_found", "No managed process has that id", [["process_id", id]]);
  if (action === "status") return renderSnapshot(callId, action, existing);
  if (action === "wait") {
    const requested = typeof args.timeout_ms === "number" && Number.isSafeInteger(args.timeout_ms)
      ? args.timeout_ms : policy.wait_timeout_ms;
    const timeoutMs = Math.min(Math.max(requested, 1), policy.max_wait_timeout_ms);
    const waited = await manager.wait(id, timeoutMs);
    const snapshot = await manager.snapshotWithLogs(
      id,
      {headBytes: policy.stdout.head_bytes, tailBytes: policy.stdout.tail_bytes},
      {headBytes: policy.stderr.head_bytes, tailBytes: policy.stderr.tail_bytes},
      policy.max_line_chars,
      policy.strip_ansi,
    );
    if (!snapshot) return executionError(callId, "not_found", "process_not_found", "No managed process has that id", [["process_id", id]]);
    return renderSnapshot(callId, action, snapshot, [["waited", waited.waited]]);
  }
  if (existing.state === "lost") return executionError(callId, "blocked", "process_lost", "The process belongs to a previous backend instance and cannot be safely signaled", [["process_id", id]]);
  await manager.stop(id);
  const stopped = await manager.snapshotWithLogs(
    id,
    {headBytes: policy.stdout.head_bytes, tailBytes: policy.stdout.tail_bytes},
    {headBytes: policy.stderr.head_bytes, tailBytes: policy.stderr.tail_bytes},
    policy.max_line_chars,
    policy.strip_ansi,
  );
  return stopped ? renderSnapshot(callId, action, stopped) : executionError(callId, "not_found", "process_not_found", "No managed process has that id", [["process_id", id]]);
}
