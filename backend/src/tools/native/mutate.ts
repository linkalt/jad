/**
 * @file mutate.ts
 * @input A resolved mutation target and the bytes to persist
 * @output Exports the shared mutation path: per-path locks, backups and atomic writes
 * @position Shared by the native `write` and `edit` tools
 *
 * Both mutating tools must offer the same durability and recovery guarantees, so
 * every guarantee lives here once: one lock table for a canonical path, a backup
 * of the previous content, a same-directory staging file, an atomic rename, and
 * quota-pruned backups. Backups collect in the configured backup directory
 * (mirrored by workspace-relative path) or beside their file when configured so.
 */

import {constants as fsConstants} from "node:fs";
import {
  chmod,
  copyFile,
  lstat,
  mkdir,
  open,
  readdir,
  realpath,
  rename,
  rm,
  type FileHandle,
} from "node:fs/promises";
import {createHash, randomBytes} from "node:crypto";
import {basename, dirname, isAbsolute, join, relative, sep} from "node:path";

import {KeyedMutex} from "../../concurrency.js";
import type {NativeMutationSettings} from "../../config.js";
import {sameFileIdentity} from "./path.js";
import type {MutateTarget} from "./path.js";

/** Backup names created by this module: `<file>.bak-<ms>-<random hex>`. */
export const BACKUP_SUFFIX_PATTERN = /^\d{13}-[0-9a-f]{16}$/;

const BACKUP_ATTEMPT_LIMIT = 5;
const IDENTIFIER_BYTES = 8;
const PROC_SELF_FD = "/proc/self/fd";

/** Mutations of one canonical path are serialized across sessions and tools. */
export const pathLocks = new KeyedMutex();

export function errorCode(error: unknown): string | undefined {
  return typeof error === "object" && error !== null && "code" in error
    ? String((error as {code?: unknown}).code)
    : undefined;
}

export interface BackupFile {
  /** Stable, model-visible backup path. */
  path: string;
  /** Descriptor-pinned path used only while the commit owns the directory. */
  operationPath: string;
  modifiedAt: number;
  size: number;
}

/** Subdirectory of a backup directory that mirrors targets outside the workspace. */
export const EXTERNAL_BACKUP_DIRECTORY_NAME = "@external";

type BackupTarget = Pick<MutateTarget, "workspaceRoot" | "realPath">;

function shortHash(value: string): string {
  return createHash("sha256").update(value).digest("hex").slice(0, 16);
}

/**
 * Directory that holds the backups of one target.
 *
 * A configured backup directory mirrors the target's own directory relative to
 * the workspace root, so nested trees stay readable and every file keeps its own
 * history; a target outside the workspace lands under `@external/<hash>`. A null
 * backup directory keeps backups beside the file they copy.
 */
export function backupDirectoryFor(
  target: BackupTarget,
  backupDir: string | null,
): string {
  if (backupDir === null) {
    return dirname(target.realPath);
  }
  const relativePath = relative(target.workspaceRoot, target.realPath);
  const insideWorkspace =
    relativePath.length > 0 &&
    relativePath !== ".." &&
    !relativePath.startsWith(`..${sep}`) &&
    !isAbsolute(relativePath);
  const mirrored = insideWorkspace
    ? dirname(relativePath)
    : join(EXTERNAL_BACKUP_DIRECTORY_NAME, shortHash(target.realPath));
  return join(backupDir, mirrored);
}

async function createBackup(
  target: BackupTarget,
  sourcePath: string,
  directory: PinnedDirectory,
  mode: number | undefined,
): Promise<string> {
  for (let attempt = 0; attempt < BACKUP_ATTEMPT_LIMIT; attempt += 1) {
    const name = `${basename(target.realPath)}.bak-${Date.now()}-${randomBytes(IDENTIFIER_BYTES).toString("hex")}`;
    const backupPath = join(directory.canonicalPath, name);
    const operationPath = join(directory.operationPath, name);
    try {
      await copyFile(sourcePath, operationPath, fsConstants.COPYFILE_EXCL);
    } catch (error) {
      if (errorCode(error) !== "EEXIST" || attempt === BACKUP_ATTEMPT_LIMIT - 1) {
        throw error;
      }
      continue;
    }
    if (mode !== undefined) {
      await chmod(operationPath, mode);
    }
    return backupPath;
  }
  throw new Error("could not create a unique backup path");
}

/** Backups of one file, newest first. Only this module's own names are matched. */
async function listBackups(
  target: BackupTarget,
  directory: PinnedDirectory,
): Promise<BackupFile[]> {
  const prefix = `${basename(target.realPath)}.bak-`;
  let entries;
  try {
    entries = await readdir(directory.operationPath, {withFileTypes: true});
  } catch {
    return [];
  }
  const backups: BackupFile[] = [];
  for (const entry of entries) {
    if (!entry.isFile() || !entry.name.startsWith(prefix)) {
      continue;
    }
    if (!BACKUP_SUFFIX_PATTERN.test(entry.name.slice(prefix.length))) {
      continue;
    }
    const backupPath = join(directory.canonicalPath, entry.name);
    const operationPath = join(directory.operationPath, entry.name);
    try {
      const stats = await lstat(operationPath);
      backups.push({path: backupPath, operationPath, modifiedAt: stats.mtimeMs, size: stats.size});
    } catch {
      continue;
    }
  }
  return backups.sort(
    (left, right) =>
      right.modifiedAt - left.modifiedAt || (left.path < right.path ? 1 : -1),
  );
}

/** Enforces the backup quota of one file; never removes the newest backup. */
async function pruneBackups(
  target: BackupTarget,
  settings: NativeMutationSettings,
  directory: PinnedDirectory,
  keepPath: string,
): Promise<void> {
  const backups = await listBackups(target, directory);
  const newest = backups.find((backup) => backup.path === keepPath);
  let retainedCount = newest ? 1 : 0;
  let retainedBytes = newest?.size ?? 0;
  const removals: string[] = [];
  for (const backup of backups) {
    if (backup.path === keepPath) {
      continue;
    }
    const withinCount = retainedCount < settings.backup_max_count;
    const withinBytes =
      settings.backup_max_total_bytes === 0 ||
      retainedBytes + backup.size <= settings.backup_max_total_bytes;
    if (withinCount && withinBytes) {
      retainedCount += 1;
      retainedBytes += backup.size;
      continue;
    }
    removals.push(backup.operationPath);
  }
  await Promise.all(
    removals.map((backupPath) => rm(backupPath, {force: true}).catch(() => undefined)),
  );
}

interface PinnedDirectory {
  canonicalPath: string;
  operationPath: string;
  handle?: FileHandle;
}

/**
 * On Linux, a /proc/self/fd path addresses the opened directory rather than
 * its current pathname. Operations through it remain in that verified
 * directory even if an attacker swaps the directory path concurrently.
 */
async function pinDirectory(directory: string): Promise<PinnedDirectory | undefined> {
  if (process.platform !== "linux") {
    return {canonicalPath: directory, operationPath: directory};
  }
  let handle: FileHandle | undefined;
  try {
    handle = await open(
      directory,
      fsConstants.O_RDONLY | fsConstants.O_DIRECTORY | fsConstants.O_NOFOLLOW,
    );
    const operationPath = `${PROC_SELF_FD}/${handle.fd}`;
    if ((await realpath(operationPath)) !== directory) {
      await handle.close();
      return undefined;
    }
    return {canonicalPath: directory, operationPath, handle};
  } catch {
    await handle?.close().catch(() => undefined);
    return undefined;
  }
}

async function pinTargetParent(targetPath: string): Promise<{
  targetPath: string;
  directory: PinnedDirectory;
} | undefined> {
  const directory = await pinDirectory(dirname(targetPath));
  return directory
    ? {targetPath: join(directory.operationPath, basename(targetPath)), directory}
    : undefined;
}

async function pinBackupSource(
  target: MutateTarget,
  targetPath: string,
): Promise<{sourcePath: string; handle: FileHandle} | undefined> {
  if (!target.identity) {
    return undefined;
  }
  let handle: FileHandle | undefined;
  try {
    handle = await open(targetPath, fsConstants.O_RDONLY | fsConstants.O_NOFOLLOW);
    const stats = await handle.stat();
    if (!stats.isFile() || !sameFileIdentity(stats, target.identity)) {
      await handle.close();
      return undefined;
    }
    return {
      sourcePath: process.platform === "linux" ? `${PROC_SELF_FD}/${handle.fd}` : targetPath,
      handle,
    };
  } catch {
    await handle?.close().catch(() => undefined);
    return undefined;
  }
}

async function pinBackupDirectory(
  target: BackupTarget,
  backupDir: string | null,
  parent: {targetPath: string; directory: PinnedDirectory},
): Promise<{directory: PinnedDirectory; owned: boolean} | undefined> {
  if (backupDir === null) {
    // The target parent is already pinned for the whole commit.
    return {
      directory: {
        canonicalPath: dirname(target.realPath),
        operationPath: dirname(parent.targetPath),
      },
      owned: false,
    };
  }
  const directory = backupDirectoryFor(target, backupDir);
  try {
    await mkdir(directory, {recursive: true});
  } catch {
    return undefined;
  }
  const pinned = await pinDirectory(directory);
  return pinned ? {directory: pinned, owned: true} : undefined;
}

/** Best effort: directory syncing is not supported on every platform. */
async function syncDirectory(directory: string): Promise<void> {
  let handle;
  try {
    handle = await open(directory, "r");
    await handle.sync();
  } catch {
    return;
  } finally {
    await handle?.close().catch(() => undefined);
  }
}

async function writeAtomically(
  targetPath: string,
  data: Buffer,
  mode: number | undefined,
  shouldSync: boolean,
  parentHandle?: FileHandle,
): Promise<void> {
  const temporaryPath = `${targetPath}.tmp-${randomBytes(IDENTIFIER_BYTES).toString("hex")}`;
  let handle;
  try {
    handle = await open(temporaryPath, "wx", mode ?? 0o666);
    await handle.writeFile(data);
    if (mode !== undefined) {
      await handle.chmod(mode);
    }
    if (shouldSync) {
      await handle.sync();
    }
    await handle.close();
    handle = undefined;
    await rename(temporaryPath, targetPath);
  } catch (error) {
    await handle?.close().catch(() => undefined);
    await rm(temporaryPath, {force: true}).catch(() => undefined);
    throw error;
  }
  if (shouldSync) {
    if (parentHandle) {
      await parentHandle.sync().catch(() => undefined);
    } else {
      await syncDirectory(dirname(targetPath));
    }
  }
}

/** Compares data using a no-follow descriptor for the inspected file identity. */
export async function hasSameContent(
  target: Pick<MutateTarget, "realPath" | "size" | "identity">,
  data: Buffer,
): Promise<boolean> {
  if (target.size !== data.length || !target.identity) {
    return false;
  }
  let handle;
  try {
    handle = await open(target.realPath, fsConstants.O_RDONLY | fsConstants.O_NOFOLLOW);
    const before = await handle.stat();
    if (!before.isFile() || !sameFileIdentity(before, target.identity)) {
      return false;
    }
    const current = await handle.readFile();
    const after = await handle.stat();
    return sameFileIdentity(after, target.identity) && current.equals(data);
  } catch {
    return false;
  } finally {
    await handle?.close().catch(() => undefined);
  }
}

/** Verifies the target still has the identity inspected before a mutation. */
async function targetStillMatches(target: MutateTarget, targetPath = target.realPath): Promise<boolean> {
  try {
    const current = await lstat(targetPath);
    return target.existed
      ? current.isFile() && target.identity !== undefined && sameFileIdentity(current, target.identity)
      : false;
  } catch (error) {
    return !target.existed && errorCode(error) === "ENOENT";
  }
}

export interface CommitResult {
  backupPath?: string;
  error?: "backup_failed" | "target_changed" | "write_failed";
  systemError?: string;
}

/**
 * Backs up the previous content, then replaces the file atomically.
 *
 * The original file is left untouched when any step fails: a failed backup
 * aborts before the write, and a failed write removes its staging file.
 */
export async function commitFile(options: {
  target: MutateTarget;
  settings: NativeMutationSettings;
  data: Buffer;
  backup: boolean;
}): Promise<CommitResult> {
  const {target, settings, data} = options;
  const parent = await pinTargetParent(target.realPath);
  if (!parent) {
    return {error: "target_changed"};
  }
  let backupDirectory: {directory: PinnedDirectory; owned: boolean} | undefined;
  let backupSource: {sourcePath: string; handle: FileHandle} | undefined;
  try {
    if (!(await targetStillMatches(target, parent.targetPath))) {
      return {error: "target_changed"};
    }
    let backupPath: string | undefined;
    if (options.backup && target.existed) {
      backupDirectory = await pinBackupDirectory(target, settings.backup_dir, parent);
      if (!backupDirectory) {
        return {error: "backup_failed"};
      }
      backupSource = await pinBackupSource(target, parent.targetPath);
      if (!backupSource) {
        return {error: "target_changed"};
      }
      try {
        backupPath = await createBackup(
          target,
          backupSource.sourcePath,
          backupDirectory.directory,
          target.mode,
        );
      } catch {
        return {error: "backup_failed"};
      }
    }

    if (!(await targetStillMatches(target, parent.targetPath))) {
      if (backupPath && backupDirectory) {
        await rm(
          join(backupDirectory.directory.operationPath, basename(backupPath)),
          {force: true},
        ).catch(() => undefined);
      }
      return {error: "target_changed"};
    }

    try {
      await writeAtomically(
        parent.targetPath,
        data,
        target.mode,
        settings.fsync,
        parent.directory.handle,
      );
    } catch (error) {
      return {
        ...(backupPath !== undefined ? {backupPath} : {}),
        error: "write_failed",
        ...(errorCode(error) !== undefined ? {systemError: errorCode(error)} : {}),
      };
    }

    if (backupPath !== undefined && backupDirectory) {
      await pruneBackups(target, settings, backupDirectory.directory, backupPath);
    }
    return backupPath !== undefined ? {backupPath} : {};
  } finally {
    await backupSource?.handle.close().catch(() => undefined);
    if (backupDirectory?.owned) {
      await backupDirectory.directory.handle?.close().catch(() => undefined);
    }
    await parent.directory.handle?.close().catch(() => undefined);
  }
}
