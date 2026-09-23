import {randomBytes} from "node:crypto";
import {mkdir, open, readFile, rename, rm} from "node:fs/promises";
import {join} from "node:path";

import {AsyncMutex} from "../../concurrency.js";
import type {NativeTaskToolSettings} from "../../config.js";

export type TaskStatus = "todo" | "in-progress" | "blocked" | "done";
export type TaskPriority = "low" | "normal" | "high" | "urgent";

export interface TaskRecord {
  id: string;
  title: string;
  status: TaskStatus;
  priority: TaskPriority;
  tags: string[];
  documents: string[];
  created_at: string;
  updated_at: string;
}

export interface TaskListFilter {
  status?: TaskStatus | "all";
  statuses?: readonly TaskStatus[];
  priority?: TaskPriority;
  tag?: string;
  limit: number;
}

export interface TaskListResult {
  tasks: TaskRecord[];
  hasMore: boolean;
}

export interface TaskPatch {
  title?: string;
  status?: TaskStatus;
  priority?: TaskPriority;
  tags?: string[];
  documents?: string[];
}

interface TaskDocument {
  schema_version: 1;
  next_id: number;
  tasks: TaskRecord[];
}

const TASK_ID_PATTERN = /^T-\d{3,}$/;
const STATUSES = new Set<TaskStatus>(["todo", "in-progress", "blocked", "done"]);
const PRIORITIES = new Set<TaskPriority>(["low", "normal", "high", "urgent"]);
export const TASK_STORAGE_FILE_NAME = "tasks.json";

export class TaskStoreError extends Error {
  public override name = "TaskStoreError";
}

function freshDocument(): TaskDocument {
  return {schema_version: 1, next_id: 1, tasks: []};
}

function now(): string {
  return new Date().toISOString();
}

function cloneTask(task: TaskRecord): TaskRecord {
  return {...task, tags: [...task.tags], documents: [...task.documents]};
}

function validTextArray(value: unknown): value is string[] {
  return Array.isArray(value) && value.every((item) => typeof item === "string");
}

function isTaskRecord(value: unknown): value is TaskRecord {
  if (typeof value !== "object" || value === null || Array.isArray(value)) return false;
  const task = value as Partial<TaskRecord>;
  return typeof task.id === "string" && TASK_ID_PATTERN.test(task.id)
    && typeof task.title === "string" && task.title.length > 0
    && typeof task.status === "string" && STATUSES.has(task.status as TaskStatus)
    && typeof task.priority === "string" && PRIORITIES.has(task.priority as TaskPriority)
    && validTextArray(task.tags) && validTextArray(task.documents)
    && typeof task.created_at === "string" && typeof task.updated_at === "string";
}

function parseDocument(value: unknown): TaskDocument {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new TaskStoreError("task storage is not a JSON object");
  }
  const document = value as Partial<TaskDocument>;
  if (document.schema_version !== 1 || !Number.isSafeInteger(document.next_id) || document.next_id! < 1 || !Array.isArray(document.tasks) || !document.tasks.every(isTaskRecord)) {
    throw new TaskStoreError("task storage has an unsupported or invalid format");
  }
  const ids = new Set<string>();
  for (const task of document.tasks) {
    if (ids.has(task.id)) throw new TaskStoreError("task storage contains duplicate task ids");
    ids.add(task.id);
  }
  return {schema_version: 1, next_id: document.next_id!, tasks: document.tasks!.map(cloneTask)};
}

function statusRank(status: TaskStatus): number {
  return {blocked: 0, "in-progress": 1, todo: 2, done: 3}[status];
}

function priorityRank(priority: TaskPriority): number {
  return {urgent: 0, high: 1, normal: 2, low: 3}[priority];
}

export class TaskStore {
  private readonly mutex = new AsyncMutex();

  public constructor(private readonly directory: string) {}

  private get path(): string { return join(this.directory, TASK_STORAGE_FILE_NAME); }

  public async create(
    input: Omit<TaskRecord, "id" | "created_at" | "updated_at">,
    policy: NativeTaskToolSettings,
  ): Promise<TaskRecord> {
    return this.mutate(policy, (document) => {
      if (document.tasks.length >= policy.max_tasks) {
        throw new TaskStoreError("the configured task capacity has been reached");
      }
      if (!Number.isSafeInteger(document.next_id) || document.next_id > Number.MAX_SAFE_INTEGER) {
        throw new TaskStoreError("task id allocation is exhausted");
      }
      const timestamp = now();
      const task: TaskRecord = {
        ...input,
        id: `T-${String(document.next_id).padStart(3, "0")}`,
        tags: [...input.tags],
        documents: [...input.documents],
        created_at: timestamp,
        updated_at: timestamp,
      };
      document.next_id += 1;
      document.tasks.push(task);
      return task;
    });
  }

  public async get(id: string): Promise<TaskRecord | undefined> {
    return this.mutex.runExclusive(async () => {
      const document = await this.load();
      const task = document.tasks.find((candidate) => candidate.id === id);
      return task ? cloneTask(task) : undefined;
    });
  }

  public async list(filter: TaskListFilter): Promise<TaskListResult> {
    return this.mutex.runExclusive(async () => {
      const document = await this.load();
      const matching = document.tasks
        .filter((task) => filter.statuses !== undefined
          ? filter.statuses.includes(task.status)
          : filter.status === "all" || (filter.status ? task.status === filter.status : task.status !== "done"))
        .filter((task) => !filter.priority || task.priority === filter.priority)
        .filter((task) => !filter.tag || task.tags.includes(filter.tag))
        .sort((left, right) => statusRank(left.status) - statusRank(right.status)
          || priorityRank(left.priority) - priorityRank(right.priority)
          || right.updated_at.localeCompare(left.updated_at)
          || left.id.localeCompare(right.id));
      return {tasks: matching.slice(0, filter.limit).map(cloneTask), hasMore: matching.length > filter.limit};
    });
  }

  public async update(id: string, patch: TaskPatch, policy: NativeTaskToolSettings): Promise<TaskRecord | undefined> {
    return this.mutate(policy, (document) => {
      const task = document.tasks.find((candidate) => candidate.id === id);
      if (!task) return undefined;
      Object.assign(task, patch, {
        ...(patch.tags ? {tags: [...patch.tags]} : {}),
        ...(patch.documents ? {documents: [...patch.documents]} : {}),
        updated_at: now(),
      });
      return task;
    });
  }

  public async delete(id: string, policy: NativeTaskToolSettings): Promise<boolean> {
    return this.mutate(policy, (document) => {
      const index = document.tasks.findIndex((task) => task.id === id);
      if (index === -1) return false;
      document.tasks.splice(index, 1);
      return true;
    });
  }

  private async mutate<T>(
    policy: NativeTaskToolSettings,
    operation: (document: TaskDocument) => T,
  ): Promise<T> {
    return this.mutex.runExclusive(async () => {
      const document = await this.load();
      const result = operation(document);
      await this.write(document, policy);
      return result;
    });
  }

  private async load(): Promise<TaskDocument> {
    let raw: string;
    try {
      raw = await readFile(this.path, "utf8");
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "ENOENT") return freshDocument();
      throw new TaskStoreError(`could not read task storage: ${error instanceof Error ? error.message : String(error)}`);
    }
    try {
      return parseDocument(JSON.parse(raw));
    } catch (error) {
      if (error instanceof TaskStoreError) throw error;
      throw new TaskStoreError("task storage is not valid JSON");
    }
  }

  private async write(document: TaskDocument, policy: NativeTaskToolSettings): Promise<void> {
    const serialized = `${JSON.stringify(document)}\n`;
    if (Buffer.byteLength(serialized, "utf8") > policy.max_storage_bytes) {
      throw new TaskStoreError("task storage exceeds the configured byte limit");
    }
    await mkdir(this.directory, {recursive: true});
    const temporary = `${this.path}.tmp-${process.pid}-${randomBytes(6).toString("hex")}`;
    let handle;
    try {
      handle = await open(temporary, "w");
      await handle.writeFile(serialized, "utf8");
      await handle.close();
      handle = undefined;
      await rename(temporary, this.path);
    } catch (error) {
      throw error instanceof TaskStoreError ? error : new TaskStoreError(`could not persist task storage: ${error instanceof Error ? error.message : String(error)}`);
    } finally {
      await handle?.close().catch(() => undefined);
      await rm(temporary, {force: true}).catch(() => undefined);
    }
  }
}
