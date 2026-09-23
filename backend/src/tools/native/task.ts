import type {NativeTaskToolSettings} from "../../config.js";
import type {JsonRecord} from "../../persistence.js";
import {executionError, renderToolResult} from "../output.js";
import type {ToolDefinition, ToolExecutionContext} from "../types.js";
import {
  TaskStore,
  TaskStoreError,
  type TaskListFilter,
  type TaskPatch,
  type TaskPriority,
  type TaskRecord,
  type TaskStatus,
} from "./task-store.js";

export const NATIVE_TASK_ID = "native.task";

export const nativeTaskDefinition: ToolDefinition = {
  id: NATIVE_TASK_ID,
  source: "native",
  name: "task",
  description: "Create and maintain session tasks.",
  inputSchema: {
    type: "object",
    properties: {
      action: {type: "string", enum: ["list", "get", "add", "update", "done", "delete"], description: "Optional when the supplied fields make the action clear."},
      id: {type: "string", minLength: 1, description: "Task id returned by add."},
      title: {type: "string", minLength: 1, description: "Task title for add or update."},
      status: {type: "string", enum: ["todo", "in-progress", "blocked", "done", "all"], description: "Task state; all is a list filter only."},
      priority: {type: "string", enum: ["low", "normal", "high", "urgent"], description: "Optional task priority or list filter. Defaults to normal when adding."},
      tags: {type: "array", items: {type: "string"}, description: "Task tags. An empty array clears tags on update."},
      documents: {type: "array", items: {type: "string"}, description: "Planning-document paths. An empty array clears links on update."},
      tag: {type: "string", minLength: 1, description: "Optional tag filter for list."},
      limit: {type: "integer", minimum: 1, description: "Optional list limit; defaults to the session policy."},
    },
    additionalProperties: false,
    allOf: [
      {if: {properties: {action: {const: "add"}}, required: ["action"]}, then: {properties: {title: {}}, required: ["title"]}},
      {if: {properties: {action: {enum: ["get", "update", "done", "delete"]}}, required: ["action"]}, then: {properties: {id: {}}, required: ["id"]}},
      {if: {properties: {action: {const: "update"}}, required: ["action"]}, then: {anyOf: [
        {properties: {title: {}}, required: ["title"]}, {properties: {status: {}}, required: ["status"]},
        {properties: {priority: {}}, required: ["priority"]}, {properties: {tags: {}}, required: ["tags"]},
        {properties: {documents: {}}, required: ["documents"]},
      ]}},
    ],
  },
};

type TaskAction = "list" | "get" | "add" | "update" | "done" | "delete";

interface TaskContext extends ToolExecutionContext {
  policy: NativeTaskToolSettings;
  store: TaskStore;
}

export async function collectTaskStateContext(store: TaskStore, options: NativeTaskToolSettings["statecontext"]): Promise<JsonRecord> {
  const listed = await store.list({statuses: options.statuses, limit: options.limit});
  return {
    items: listed.tasks.map(({id, title, status, priority, tags, updated_at}) => ({id, title, status, priority, tags, updated_at})),
    has_more: listed.hasMore,
  };
}

const ACTIONS: Record<string, TaskAction> = {
  list: "list", get: "get", add: "add", create: "add", new: "add", update: "update", edit: "update", done: "done", complete: "done", finish: "done", delete: "delete", remove: "delete",
};

function text(value: unknown): string | undefined {
  return typeof value === "string" ? value.trim() : undefined;
}

function normalizedStatus(value: unknown): string | undefined {
  const input = text(value)?.toLowerCase().replace(/[ _]+/g, "-");
  if (input === "inprogress") return "in-progress";
  if (input === "complete" || input === "completed") return "done";
  return input;
}

function normalizedPriority(value: unknown): string | undefined {
  return text(value)?.toLowerCase();
}

function normalizedArray(value: unknown): unknown {
  if (!Array.isArray(value)) return value;
  const seen = new Set<string>();
  const values: unknown[] = [];
  for (const item of value) {
    if (typeof item !== "string") {
      values.push(item);
      continue;
    }
    const normalized = item.trim();
    if (normalized.length > 0 && !seen.has(normalized)) {
      seen.add(normalized);
      values.push(normalized);
    }
  }
  return values;
}

export function normalizeTaskArguments(args: JsonRecord): JsonRecord {
  const normalized: JsonRecord = {...args};
  const rawAction = text(args.action)?.toLowerCase().replace(/[ _-]+/g, "");
  if (rawAction) normalized.action = ACTIONS[rawAction] ?? rawAction;
  if (normalized.id === undefined && typeof args.taskId === "string") normalized.id = args.taskId;
  if (normalized.limit === undefined && args.maxItems !== undefined) normalized.limit = args.maxItems;
  if (typeof normalized.limit === "number" && Number.isSafeInteger(normalized.limit)) normalized.limit = Math.max(1, normalized.limit);
  const id = text(normalized.id);
  if (id !== undefined) normalized.id = id.toUpperCase();
  const title = text(normalized.title);
  if (title !== undefined) normalized.title = title;
  const status = normalizedStatus(normalized.status);
  if (status !== undefined) normalized.status = status;
  const priority = normalizedPriority(normalized.priority);
  if (priority !== undefined) normalized.priority = priority;
  const tag = normalizedPriority(normalized.tag);
  if (tag !== undefined) normalized.tag = tag;
  if (normalized.tags !== undefined) {
    const values = normalizedArray(normalized.tags);
    normalized.tags = Array.isArray(values) ? [...new Set(values.map((value) => typeof value === "string" ? value.toLowerCase() : value))] : values;
  }
  if (normalized.documents !== undefined) normalized.documents = normalizedArray(normalized.documents);
  delete normalized.taskId;
  delete normalized.maxItems;
  if (normalized.action === undefined) {
    const hasId = typeof normalized.id === "string" && normalized.id.length > 0;
    const hasPatch = ["title", "status", "priority", "tags", "documents"].some((key) => normalized[key] !== undefined);
    normalized.action = hasId ? (hasPatch ? "update" : "get") : (normalized.title !== undefined ? "add" : "list");
  }
  return normalized;
}

function status(value: unknown): TaskStatus | undefined {
  return value === "todo" || value === "in-progress" || value === "blocked" || value === "done" ? value : undefined;
}

function priority(value: unknown): TaskPriority | undefined {
  return value === "low" || value === "normal" || value === "high" || value === "urgent" ? value : undefined;
}

function validText(value: unknown, maximum: number): value is string {
  return typeof value === "string" && value.trim().length > 0 && value.length <= maximum;
}

function stringArray(value: unknown, maximumItems: number, maximumChars: number): string[] | undefined {
  if (!Array.isArray(value) || value.length > maximumItems || !value.every((item) => validText(item, maximumChars))) return undefined;
  return value as string[];
}

function actionFor(args: JsonRecord): TaskAction | undefined {
  return typeof args.action === "string" && ["list", "get", "add", "update", "done", "delete"].includes(args.action)
    ? args.action as TaskAction
    : undefined;
}

function failure(callId: string, code: string, summary: string, information: Array<[string, unknown]> = []): string {
  return executionError(callId, "invalid_input", code, summary, information);
}

function taskInformation(task: TaskRecord, action: TaskAction): Array<[string, unknown]> {
  return [
    ["action", action],
    ["task_id", task.id],
    ["title", task.title],
    ["status", task.status],
    ["priority", task.priority],
    ...(task.tags.length > 0 ? [["tags", task.tags] as [string, unknown]] : []),
    ...(task.documents.length > 0 ? [["documents", task.documents] as [string, unknown]] : []),
  ];
}

function renderTask(callId: string, action: TaskAction, task: TaskRecord, summary: string): string {
  return renderToolResult({id: callId, status: "ok", summary, information: taskInformation(task, action)});
}

function noExtra(args: JsonRecord, allowed: readonly string[]): boolean {
  return Object.keys(args).every((key) => allowed.includes(key));
}

export async function executeNativeTask(args: JsonRecord, context: TaskContext): Promise<string> {
  args = normalizeTaskArguments(args);
  const action = actionFor(args);
  if (!action) return failure(context.callId, "invalid_action", "Task action is invalid");
  const id = text(args.id)?.toUpperCase();
  const taskStatus = status(args.status);
  const taskPriority = priority(args.priority);
  const title = text(args.title);
  const tags = args.tags === undefined ? undefined : stringArray(args.tags, context.policy.max_tags, context.policy.max_tag_chars);
  const documents = args.documents === undefined ? undefined : stringArray(args.documents, context.policy.max_documents, context.policy.max_document_chars);

  if (title !== undefined && !validText(title, context.policy.max_title_chars)) return failure(context.callId, "invalid_title", "Task title is empty or exceeds the configured length");
  if (args.tags !== undefined && !tags) return failure(context.callId, "invalid_tags", "Task tags exceed the configured limits or contain empty values");
  if (args.documents !== undefined && !documents) return failure(context.callId, "invalid_documents", "Task documents exceed the configured limits or contain empty values");

  try {
    if (action === "list") {
      if (!noExtra(args, ["action", "status", "priority", "tag", "limit"])) return failure(context.callId, "invalid_list", "List accepts only status, priority, tag, and limit filters");
      if (args.status !== undefined && args.status !== "all" && !taskStatus) return failure(context.callId, "invalid_status", "Task status is invalid");
      if (args.priority !== undefined && !taskPriority) return failure(context.callId, "invalid_priority", "Task priority is invalid");
      const requestedLimit = typeof args.limit === "number" && Number.isSafeInteger(args.limit) ? args.limit : context.policy.list_limit;
      const limit = Math.min(Math.max(1, requestedLimit), context.policy.max_list_limit);
      const listed = await context.store.list({
        ...(args.status === "all" ? {status: "all" as const} : taskStatus ? {status: taskStatus} : {}),
        ...(taskPriority ? {priority: taskPriority} : {}),
        ...(typeof args.tag === "string" ? {tag: args.tag} : {}),
        limit,
      } satisfies TaskListFilter);
      const lines: string[] = [];
      for (const task of listed.tasks) {
        const line = `${task.id} | ${task.status} | ${task.priority} | ${task.title}${task.tags.length > 0 ? ` | tags: ${task.tags.join(",")}` : ""}${task.documents.length > 0 ? ` | documents: ${task.documents.length}` : ""}`;
        if (Buffer.byteLength([...lines, line].join("\n"), "utf8") > context.policy.max_list_bytes) break;
        lines.push(line);
      }
      const hasMore = listed.hasMore || lines.length < listed.tasks.length;
      return renderToolResult({
        id: context.callId,
        status: "ok",
        summary: `${lines.length} task${lines.length === 1 ? "" : "s"} listed`,
        information: [["action", "list"], ["count", lines.length], ...(hasMore ? [["has_more", true] as [string, unknown]] : [])],
        ...(lines.length > 0 ? {content: {encoding: "utf8", text: lines.join("\n")}} : {}),
      });
    }

    if (action === "add") {
      if (!noExtra(args, ["action", "title", "status", "priority", "tags", "documents"])) return failure(context.callId, "invalid_add", "Add accepts title, todo or in-progress status, priority, tags, and documents");
      if (!title) return failure(context.callId, "missing_title", "Add requires a title");
      if (args.status !== undefined && (taskStatus !== "todo" && taskStatus !== "in-progress")) return failure(context.callId, "invalid_creation_status", "New tasks must be todo or in-progress");
      if (args.priority !== undefined && !taskPriority) return failure(context.callId, "invalid_priority", "Task priority is invalid");
      const task = await context.store.create({
        title,
        status: taskStatus ?? "todo",
        priority: taskPriority ?? "normal",
        tags: tags ?? [],
        documents: documents ?? [],
      }, context.policy);
      return renderTask(context.callId, action, task, "Task added");
    }

    if (!id) return failure(context.callId, "missing_task_id", "This task action requires an id");
    if (!/^T-\d{3,}$/.test(id)) return failure(context.callId, "invalid_task_id", "Task id is invalid");
    if (action === "get") {
      if (!noExtra(args, ["action", "id"])) return failure(context.callId, "invalid_get", "Get accepts only an id");
      const task = await context.store.get(id);
      return task ? renderTask(context.callId, action, task, "Task retrieved") : executionError(context.callId, "not_found", "task_not_found", "No task has that id", [["task_id", id]]);
    }
    if (action === "delete") {
      if (!noExtra(args, ["action", "id"])) return failure(context.callId, "invalid_delete", "Delete requires exactly an id");
      const deleted = await context.store.delete(id, context.policy);
      return deleted ? renderToolResult({id: context.callId, status: "ok", summary: "Task deleted", information: [["action", action], ["task_id", id]]}) : executionError(context.callId, "not_found", "task_not_found", "No task has that id", [["task_id", id]]);
    }
    if (action === "done") {
      if (!noExtra(args, ["action", "id"])) return failure(context.callId, "invalid_done", "Done requires exactly an id");
      const task = await context.store.update(id, {status: "done"}, context.policy);
      return task ? renderTask(context.callId, action, task, "Task completed") : executionError(context.callId, "not_found", "task_not_found", "No task has that id", [["task_id", id]]);
    }
    if (!noExtra(args, ["action", "id", "title", "status", "priority", "tags", "documents"])) return failure(context.callId, "invalid_update", "Update accepts title, status, priority, tags, and documents");
    if (args.status !== undefined && !taskStatus) return failure(context.callId, "invalid_status", "Task status is invalid");
    if (args.priority !== undefined && !taskPriority) return failure(context.callId, "invalid_priority", "Task priority is invalid");
    const patch: TaskPatch = {
      ...(title !== undefined ? {title} : {}),
      ...(taskStatus ? {status: taskStatus} : {}),
      ...(taskPriority ? {priority: taskPriority} : {}),
      ...(tags ? {tags} : {}),
      ...(documents ? {documents} : {}),
    };
    if (Object.keys(patch).length === 0 || args.status === "all") return failure(context.callId, "missing_update", "Update needs a title, status, priority, tags, or documents");
    const task = await context.store.update(id, patch, context.policy);
    return task ? renderTask(context.callId, action, task, "Task updated") : executionError(context.callId, "not_found", "task_not_found", "No task has that id", [["task_id", id]]);
  } catch (error) {
    if (error instanceof TaskStoreError) return executionError(context.callId, "error", "task_storage_error", error.message);
    return executionError(context.callId, "error", "task_operation_failed", error instanceof Error ? error.message : String(error));
  }
}
