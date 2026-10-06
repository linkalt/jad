/**
 * @file checkpoint.ts
 * @input Checkpoint settings, the workspace (`cwd`), the session staging directory
 * @output CheckpointService: list, per-run snapshots, restore (safe or full, optionally a dry run)
 * @position Between the run loop / HTTP routes and ShadowGit; see design_checkpoint.md
 *
 * A checkpoint records the workspace before a run changes it (`before_tree`) and
 * after the run ended (`after_tree`). Restoring reverts what the run changed.
 * The safe mode leaves a path alone when the user changed it after the run, so
 * it never overwrites later work; the full mode makes the workspace equal to
 * `before_tree`. A restore first records its own checkpoint, so it can be undone.
 *
 * A checkpoint is a safety net, not a gate: a snapshot that fails or is skipped
 * is recorded with a warning and the tool call it preceded still runs.
 * messages.jsonl is never written; the records live in `checkpoints.json`.
 */

import {randomBytes} from "node:crypto";
import {mkdir, readFile, rm} from "node:fs/promises";
import {homedir} from "node:os";
import {dirname, isAbsolute, join, parse, relative, resolve, sep} from "node:path";

import {GitError, ShadowGit, type TreeChange} from "./checkpoint-git.js";
import type {CheckpointSettings, LoadedSessionConfig} from "./config.js";
import {AsyncMutex} from "./concurrency.js";
import {writeJsonAtomic, type DurabilityMode, type JsonRecord} from "./persistence.js";
import {callNeedsApproval} from "./tools/approval.js";

export const CHECKPOINT_METADATA_FILE = "checkpoints.json";
export const CHECKPOINT_GIT_DIRECTORY = "checkpoints.git";
const SCHEMA_VERSION = 1;
const REF_PREFIX = "refs/checkpoints/";
/** Largest single git output accepted: a listing or diff of a workspace at the file-count ceiling. */
const MAX_GIT_OUTPUT_BYTES = 256 * 1024 * 1024;
/** Backups written by write/edit beside their file; restoring must not roll the safety copies back. */
const BACKUP_FILE_PATTERN = "*.bak-[0-9]*-*";

export type CheckpointKind = "run" | "pre_restore";
/**
 * `interrupted` is never stored: the list reports it for a record that is still
 * `open` while no run of the session is live (the server stopped mid-run).
 */
export type CheckpointState = "open" | "complete" | "skipped" | "failed" | "interrupted";
export type RestoreMode = "safe" | "full";

export interface CheckpointRecord {
  id: string;
  kind: CheckpointKind;
  turn_id: string;
  session_turn: number;
  created_at: string;
  finished_at?: string;
  state: CheckpointState;
  /** The workspace before the run changed it. Absent when the snapshot was skipped or failed. */
  before_tree?: string;
  /** The workspace when the run ended. Absent while the run is open or when it ended abnormally. */
  after_tree?: string;
  file_count?: number;
  repository_bytes?: number;
  /** Why the snapshot was skipped or failed, or what a restore did. */
  warning?: string;
  /** For `pre_restore`: the checkpoint that was restored. */
  restored_from?: string;
}

export interface RestoreResult {
  checkpoint_id: string;
  mode: RestoreMode;
  dry_run: boolean;
  restored: string[];
  deleted: string[];
  conflicts: string[];
  /** The checkpoint that undoes this restore; absent for a dry run or when nothing changed. */
  undo_checkpoint_id?: string;
}

export type CheckpointErrorCode = "not_found" | "conflict" | "unavailable";

export class CheckpointError extends Error {
  public constructor(
    public readonly code: CheckpointErrorCode,
    message: string,
  ) {
    super(message);
    this.name = "CheckpointError";
  }
}

/** A snapshot that was not taken on purpose (a limit), as opposed to one that failed. */
class SnapshotSkipped extends Error {}

export interface CheckpointServiceOptions {
  settings: CheckpointSettings;
  /** The directory the agent works in; its files are what a checkpoint covers. */
  workspace: string;
  /** Where `checkpoints.json` and `checkpoints.git` live. */
  stagingDir: string;
  /** Absolute paths owned by the harness that sit inside the workspace and must never be snapshotted. */
  harnessPaths: readonly string[];
  durability: DurabilityMode;
}

function escapeGitPattern(path: string): string {
  return path.replace(/[\\*?[\]]/g, (character) => `\\${character}`).replace(/^([!#])/, "\\$1");
}

/** Exclusion patterns for the private repository: harness files, and backups made by write/edit. */
export function exclusionPatterns(workspace: string, harnessPaths: readonly string[]): string[] {
  const patterns = new Set<string>([BACKUP_FILE_PATTERN]);
  for (const path of harnessPaths) {
    const inside = relative(workspace, path);
    if (inside === "" || inside.startsWith("..") || isAbsolute(inside)) continue;
    patterns.add(`/${escapeGitPattern(inside.split(sep).join("/"))}`);
  }
  return [...patterns];
}

/** Absolute harness-owned paths of a session that may fall inside its workspace. */
export function harnessPathsOf(config: Pick<LoadedSessionConfig, "settings" | "paths">): string[] {
  const paths = config.paths;
  const native = config.settings.tools.native;
  const candidates: Array<string | undefined | null> = [
    paths.staging_dir, paths.messages_path, paths.messages_backup_path, paths.setting_path, paths.model_path,
    paths.http_debug_dir, paths.compaction_cache_path, paths.history_cache_path, paths.usage_cache_path,
    paths.cursor_cache_path, paths.run_error_cache_path, paths.statecontext_path,
    native.task?.storage_dir, native.memory?.storage_dir, native.fetch?.storage_dir, native.process?.storage_dir,
    native.bash?.spill?.storage_dir, native.write?.backup_dir, native.edit?.backup_dir,
  ];
  // The session directory itself is harness-owned unless the workspace is that directory.
  if (resolve(paths.session_dir) !== resolve(config.settings.cwd)) candidates.push(paths.session_dir);
  return candidates
    .filter((value): value is string => typeof value === "string" && value.length > 0)
    .map((value) => resolve(config.settings.cwd, value));
}

/** Whether a call can change files or start something that can; reads never take a checkpoint. */
export function callChangesWorkspace(
  call: JsonRecord,
  source: "native" | "mcp" | undefined,
  includeMcpTools: boolean,
): boolean {
  if (source === "mcp") return includeMcpTools;
  const name = typeof call.name === "string" ? call.name : "";
  if (name === "write" || name === "edit" || name === "bash") return true;
  if (name !== "process") return false;
  let args: unknown;
  try {
    args = typeof call.arguments === "string" ? JSON.parse(call.arguments) : undefined;
  } catch {
    return false;
  }
  return typeof args === "object" && args !== null && !Array.isArray(args)
    && callNeedsApproval("process", args as JsonRecord);
}

/** The checkpoint service of one session, built from its settings and storage paths. */
export function checkpointServiceFor(
  config: Pick<LoadedSessionConfig, "settings" | "paths">,
): CheckpointService {
  return new CheckpointService({
    settings: config.settings.checkpoint,
    workspace: config.settings.cwd,
    stagingDir: config.paths.staging_dir,
    harnessPaths: harnessPathsOf(config),
    durability: config.settings.persistence.durability,
  });
}

function newCheckpointId(): string {
  return `ckpt-${Date.now().toString(36)}-${randomBytes(4).toString("hex")}`;
}

function describe(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

/** The text shown for a record cut short; `before_tree` is kept, so a full restore still works. */
export const INTERRUPTED_WARNING = "The run ended without recording its end (the server stopped or crashed).";

function interrupted(record: CheckpointRecord): CheckpointRecord {
  return {...record, state: "interrupted", warning: record.warning ?? INTERRUPTED_WARNING};
}

export class CheckpointService {
  private readonly mutex = new AsyncMutex();
  private readonly metadataPath: string;
  private readonly git: ShadowGit;

  public constructor(private readonly options: CheckpointServiceOptions) {
    this.metadataPath = join(options.stagingDir, CHECKPOINT_METADATA_FILE);
    this.git = new ShadowGit({
      gitDir: join(options.stagingDir, CHECKPOINT_GIT_DIRECTORY),
      workTree: options.workspace,
      binary: options.settings.git_binary,
      timeoutMs: options.settings.timeout_ms,
      maxOutputBytes: MAX_GIT_OUTPUT_BYTES,
    });
  }

  public get enabled(): boolean {
    return this.options.settings.enabled;
  }

  public get snapshotOn(): CheckpointSettings["snapshot_on"] {
    return this.options.settings.snapshot_on;
  }

  public get includeMcpTools(): boolean {
    return this.options.settings.include_mcp_tools;
  }

  // ---- metadata ---------------------------------------------------------

  private async readAll(): Promise<CheckpointRecord[]> {
    let text: string;
    try {
      text = await readFile(this.metadataPath, "utf8");
    } catch {
      return [];
    }
    try {
      const parsed = JSON.parse(text) as {checkpoints?: unknown};
      return Array.isArray(parsed.checkpoints) ? parsed.checkpoints as CheckpointRecord[] : [];
    } catch {
      // A damaged file only loses the list; the snapshots stay in the private repository.
      return [];
    }
  }

  private async writeAll(records: CheckpointRecord[]): Promise<void> {
    await mkdir(dirname(this.metadataPath), {recursive: true});
    await writeJsonAtomic(
      this.metadataPath,
      {schema_version: SCHEMA_VERSION, checkpoints: records},
      this.options.durability,
    );
  }

  /**
   * Newest first. Reading never needs git, so a missing binary does not hide the list.
   *
   * A record still `open` while no run is live was cut short (the server stopped or
   * crashed before `finish`), so it is reported as `interrupted`. That is decided at
   * read time and never written: the service is built per request and cannot know
   * which runs are live, and a write here could race with a run that has just begun.
   */
  public async list(options: {runIsLive?: boolean} = {}): Promise<CheckpointRecord[]> {
    const records = (await this.readAll()).slice().reverse();
    if (options.runIsLive !== false) return records;
    return records.map((record) => (record.state === "open" ? interrupted(record) : record));
  }

  // ---- snapshots ----------------------------------------------------------

  private assertWorkspaceAllowed(): void {
    const workspace = resolve(this.options.workspace);
    if (workspace === parse(workspace).root || workspace === resolve(homedir())) {
      throw new SnapshotSkipped(`the workspace ${workspace} is a filesystem root or the home directory; set a narrower cwd to use checkpoints`);
    }
  }

  /** Stages the workspace and returns its tree, or throws SnapshotSkipped over a limit. */
  private async capture(): Promise<{tree: string; fileCount: number; bytes: number}> {
    this.assertWorkspaceAllowed();
    await this.git.prepare(exclusionPatterns(this.options.workspace, this.options.harnessPaths));
    const before = await this.git.repositoryBytes();
    if (before > this.options.settings.max_total_bytes) {
      throw new SnapshotSkipped(`the checkpoint repository holds ${before} bytes, over checkpoint.max_total_bytes (${this.options.settings.max_total_bytes})`);
    }
    const {tree, fileCount} = await this.git.snapshot();
    if (fileCount > this.options.settings.max_files) {
      throw new SnapshotSkipped(`the workspace has ${fileCount} files, over checkpoint.max_files (${this.options.settings.max_files})`);
    }
    return {tree, fileCount, bytes: await this.git.repositoryBytes()};
  }

  private async keep(id: string, suffix: "before" | "after", tree: string, message: string): Promise<void> {
    const commit = await this.git.commit(tree, message);
    await this.git.setRef(`${REF_PREFIX}${id}-${suffix}`, commit);
  }

  private async prune(records: CheckpointRecord[]): Promise<CheckpointRecord[]> {
    const excess = records.length - this.options.settings.max_checkpoints;
    if (excess <= 0) return records;
    for (const dropped of records.slice(0, excess)) {
      for (const suffix of ["before", "after"]) {
        await this.git.deleteRef(`${REF_PREFIX}${dropped.id}-${suffix}`).catch(() => undefined);
      }
    }
    return records.slice(excess);
  }

  /** Records the state before a run changes anything. Never throws: a failure is a record. */
  public async begin(turnId: string, sessionTurn: number): Promise<string | undefined> {
    return this.mutex.runExclusive(async () => {
      const record: CheckpointRecord = {
        id: newCheckpointId(),
        kind: "run",
        turn_id: turnId,
        session_turn: sessionTurn,
        created_at: new Date().toISOString(),
        state: "open",
      };
      try {
        const captured = await this.capture();
        await this.keep(record.id, "before", captured.tree, `before ${turnId}`);
        record.before_tree = captured.tree;
        record.file_count = captured.fileCount;
        record.repository_bytes = captured.bytes;
      } catch (error) {
        record.state = error instanceof SnapshotSkipped ? "skipped" : "failed";
        record.warning = `Checkpoint not taken: ${describe(error)}`;
        record.finished_at = record.created_at;
      }
      try {
        await this.writeAll(await this.prune([...await this.readAll(), record]));
      } catch (error) {
        // Without the record there is nothing to finish or restore later.
        console.error(`[checkpoint] cannot record checkpoint ${record.id}: ${describe(error)}`);
        return undefined;
      }
      return record.state === "open" ? record.id : undefined;
    });
  }

  /** Records the state when the run ended. Never throws. */
  public async finish(id: string): Promise<void> {
    await this.mutex.runExclusive(async () => {
      try {
        const records = await this.readAll();
        const record = records.find((entry) => entry.id === id);
        if (record === undefined) return;
        try {
          const captured = await this.capture();
          await this.keep(record.id, "after", captured.tree, `after ${record.turn_id}`);
          record.after_tree = captured.tree;
          record.file_count = captured.fileCount;
          record.repository_bytes = captured.bytes;
          record.state = "complete";
        } catch (error) {
          record.state = error instanceof SnapshotSkipped ? "skipped" : "failed";
          record.warning = `The end of the run was not recorded: ${describe(error)}`;
        }
        record.finished_at = new Date().toISOString();
        await this.writeAll(records);
      } catch (error) {
        console.error(`[checkpoint] cannot finish checkpoint ${id}: ${describe(error)}`);
      }
    });
  }

  // ---- restore --------------------------------------------------------------

  public async restore(
    id: string,
    options: {dryRun: boolean; mode?: RestoreMode},
  ): Promise<RestoreResult> {
    return this.mutex.runExclusive(async () => {
      const records = await this.readAll();
      const record = records.find((entry) => entry.id === id);
      if (record === undefined) throw new CheckpointError("not_found", `checkpoint ${id} does not exist`);
      if (record.before_tree === undefined) {
        throw new CheckpointError("conflict", `checkpoint ${id} has no saved state to restore (${record.warning ?? record.state})`);
      }
      const mode: RestoreMode = options.mode ?? (this.options.settings.safe_restore ? "safe" : "full");
      if (mode === "safe" && record.after_tree === undefined) {
        throw new CheckpointError(
          "conflict",
          `checkpoint ${id} has no run-end snapshot, so a safe restore cannot tell what the run changed; use mode "full"`,
        );
      }
      try {
        await this.git.prepare(exclusionPatterns(this.options.workspace, this.options.harnessPaths));
        const current = await this.capture();
        const plan = await this.plan(record, current.tree, mode);
        const result: RestoreResult = {
          checkpoint_id: id,
          mode,
          dry_run: options.dryRun,
          restored: plan.restore.map((change) => change.path),
          deleted: plan.remove.map((change) => change.path),
          conflicts: plan.conflicts,
        };
        if (options.dryRun || (plan.restore.length === 0 && plan.remove.length === 0)) return result;

        const undo: CheckpointRecord = {
          id: newCheckpointId(),
          kind: "pre_restore",
          turn_id: `restore-${id}`,
          session_turn: record.session_turn,
          created_at: new Date().toISOString(),
          state: "open",
          before_tree: current.tree,
          restored_from: id,
        };
        await this.keep(undo.id, "before", current.tree, `before restore of ${id}`);
        await this.git.checkout(record.before_tree, plan.restore.map((change) => change.path));
        for (const change of plan.remove) {
          const target = resolve(this.options.workspace, change.path);
          if (!target.startsWith(`${resolve(this.options.workspace)}${sep}`)) continue;
          await rm(target, {force: true});
        }
        const after = await this.capture();
        await this.keep(undo.id, "after", after.tree, `after restore of ${id}`);
        undo.after_tree = after.tree;
        undo.file_count = after.fileCount;
        undo.repository_bytes = after.bytes;
        undo.state = "complete";
        undo.finished_at = new Date().toISOString();
        undo.warning = `Restored ${plan.restore.length} and deleted ${plan.remove.length} path(s) from ${id}`
          + (plan.conflicts.length > 0 ? `; left ${plan.conflicts.length} changed path(s) alone` : "");
        await this.writeAll(await this.prune([...await this.readAll(), undo]));
        result.undo_checkpoint_id = undo.id;
        return result;
      } catch (error) {
        if (error instanceof CheckpointError) throw error;
        if (error instanceof SnapshotSkipped) throw new CheckpointError("unavailable", describe(error));
        if (error instanceof GitError) throw new CheckpointError("unavailable", describe(error));
        throw error;
      }
    });
  }

  /** Which paths to write back, which to delete, and which the user changed after the run. */
  private async plan(
    record: CheckpointRecord,
    currentTree: string,
    mode: RestoreMode,
  ): Promise<{restore: TreeChange[]; remove: TreeChange[]; conflicts: string[]}> {
    const before = record.before_tree as string;
    if (mode === "full") {
      // Turn the current workspace into `before`: what exists only now goes, the rest is written back.
      const changes = await this.git.diff(currentTree, before);
      return {
        restore: changes.filter((change) => change.status !== "D"),
        remove: changes.filter((change) => change.status === "D"),
        conflicts: [],
      };
    }
    const after = record.after_tree as string;
    const ranChanges = await this.git.diff(before, after);
    const changedSince = new Set((await this.git.diff(after, currentTree)).map((change) => change.path));
    const restore: TreeChange[] = [];
    const remove: TreeChange[] = [];
    const conflicts: string[] = [];
    for (const change of ranChanges) {
      if (changedSince.has(change.path)) {
        conflicts.push(change.path);
      } else if (change.status === "A") {
        remove.push(change);
      } else {
        restore.push(change);
      }
    }
    return {restore, remove, conflicts};
  }
}

export interface CheckpointRunHooks {
  /** For `snapshot_on: run_start`: takes the checkpoint before the first provider request. */
  start(): Promise<void>;
  /** Called before the calls of one tool round run; for `first_mutation` it takes the checkpoint at the first changing call. */
  beforeCalls(changesWorkspace: boolean): Promise<void>;
  /** Called when the run ends, whatever the outcome. */
  finish(): Promise<void>;
}

/** The checkpoint of one run: taken at most once, closed once. */
export function createRunCheckpoint(
  service: CheckpointService,
  turnId: string,
  sessionTurn: number,
): CheckpointRunHooks {
  let id: string | undefined;
  let attempted = false;
  const take = async (): Promise<void> => {
    if (attempted) return;
    attempted = true;
    id = await service.begin(turnId, sessionTurn);
  };
  return {
    async start(): Promise<void> {
      if (service.snapshotOn === "run_start") await take();
    },
    async beforeCalls(changesWorkspace: boolean): Promise<void> {
      if (changesWorkspace && service.snapshotOn === "first_mutation") await take();
    },
    async finish(): Promise<void> {
      if (id !== undefined) await service.finish(id);
    },
  };
}
