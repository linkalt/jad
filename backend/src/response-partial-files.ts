import {mkdir, open, readdir, readFile, rename, rm, stat} from "node:fs/promises";
import type {Dirent} from "node:fs";
import type {FileHandle} from "node:fs/promises";
import {randomUUID} from "node:crypto";
import {hostname} from "node:os";
import {dirname, join} from "node:path";

export const RESPONSE_PARTIAL_FILE = "responses.partial";
export const RESPONSE_PARTIAL_OWNER_FILE = "responses.partial.owner";
export const RESPONSE_PARTIAL_COMPACT_FILE = "responses.partial.compact";
export const RESPONSE_PARTIAL_CLEANUP_LOCK_FILE = "responses.partial.cleanup-lock";

interface ResponsePartialOwner {
  hostname: string;
  pid: number;
  token?: string;
}

/** Acquires an exclusive process lease before creating the ephemeral content file. */
export async function acquireResponsePartialLease(partialPath: string): Promise<void> {
  const directory = dirname(partialPath);
  const ownerPath = `${partialPath}.owner`;
  const cleanupLockPath = join(directory, RESPONSE_PARTIAL_CLEANUP_LOCK_FILE);
  const owner: ResponsePartialOwner = {hostname: hostname(), pid: process.pid};
  await mkdir(directory, {recursive: true});
  if (await fileExists(cleanupLockPath)) {
    throw new Error("response partial cache cleanup is in progress");
  }

  let handle;
  try {
    handle = await open(ownerPath, "wx", 0o600);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "EEXIST") throw error;
    const existing = await readOwner(ownerPath);
    throw new Error(existing !== undefined && ownerIsAlive(existing)
      ? "response partial cache is owned by a live process"
      : "response partial cache has a stale owner and must be scavenged before reuse");
  }

  try {
    await handle.writeFile(`${JSON.stringify(owner)}\n`, "utf8");
    await handle.close();
  } catch (error) {
    await handle.close().catch(() => undefined);
    await rm(ownerPath, {force: true}).catch(() => undefined);
    throw error;
  }

  // A startup scavenger that raced the lease creation must win before any data
  // file is opened; otherwise it observes our live PID and leaves the cache alone.
  try {
    if (await fileExists(cleanupLockPath)) throw new Error("response partial cache cleanup is in progress");
  } catch (error) {
    await rm(ownerPath, {force: true}).catch(() => undefined);
    throw error;
  }
}

export async function releaseResponsePartialLease(partialPath: string): Promise<void> {
  await rm(`${partialPath}.owner`, {force: true});
}

/** Removes only cache files whose recorded local process has exited. */
export async function cleanupOrphanedResponsePartialFiles(stagingDirectory: string): Promise<number> {
  let entries: Dirent[];
  try {
    entries = await readdir(stagingDirectory, {withFileTypes: true});
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return 0;
    throw error;
  }

  let removed = 0;
  for (const entry of entries) {
    if (!entry.isDirectory()) continue;
    const partialPath = join(stagingDirectory, entry.name, RESPONSE_PARTIAL_FILE);
    const ownerPath = `${partialPath}.owner`;
    if (!(await fileExists(ownerPath))) continue;
    const cleanupLockPath = join(stagingDirectory, entry.name, RESPONSE_PARTIAL_CLEANUP_LOCK_FILE);
    const lockToken = await acquireCleanupLock(cleanupLockPath);
    if (lockToken === undefined) continue;
    try {
      const owner = await readOwner(ownerPath);
      if (owner === undefined || ownerIsAlive(owner)) continue;
      await removePartialFiles(partialPath);
      removed += 1;
    } finally {
      await releaseCleanupLock(cleanupLockPath, lockToken);
    }
  }
  return removed;
}

async function acquireCleanupLock(path: string): Promise<string | undefined> {
  for (let attempt = 0; attempt < 2; attempt += 1) {
    let handle: FileHandle;
    try {
      handle = await open(path, "wx", 0o600);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "EEXIST") throw error;
      const existing = await readOwner(path);
      if (existing !== undefined && ownerIsAlive(existing)) return undefined;
      const stalePath = `${path}.stale-${randomUUID()}`;
      try {
        await rename(path, stalePath);
      } catch (renameError) {
        if ((renameError as NodeJS.ErrnoException).code === "ENOENT") continue;
        throw renameError;
      }
      await rm(stalePath, {force: true});
      continue;
    }

    const token = randomUUID();
    try {
      await handle.writeFile(`${JSON.stringify({hostname: hostname(), pid: process.pid, token})}\n`, "utf8");
      await handle.close();
    } catch (error) {
      await handle.close().catch(() => undefined);
      await rm(path, {force: true}).catch(() => undefined);
      throw error;
    }
    if ((await readOwner(path))?.token === token) return token;
    return undefined;
  }
  return undefined;
}

async function releaseCleanupLock(path: string, token: string): Promise<void> {
  if ((await readOwner(path))?.token === token) await rm(path, {force: true});
}

async function readOwner(path: string): Promise<ResponsePartialOwner | undefined> {
  let source: string;
  try {
    source = await readFile(path, "utf8");
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return undefined;
    throw error;
  }

  try {
    const value: unknown = JSON.parse(source);
    if (
      typeof value !== "object" || value === null ||
      !Number.isSafeInteger((value as ResponsePartialOwner).pid) ||
      (value as ResponsePartialOwner).pid <= 0 ||
      typeof (value as ResponsePartialOwner).hostname !== "string" ||
      ((value as ResponsePartialOwner).token !== undefined &&
        typeof (value as ResponsePartialOwner).token !== "string")
    ) {
      return undefined;
    }
    return value as ResponsePartialOwner;
  } catch {
    return undefined;
  }
}

function ownerIsAlive(owner: ResponsePartialOwner): boolean {
  // A different host cannot prove the process is gone, so preserve its cache.
  if (owner.hostname !== hostname()) return true;
  try {
    process.kill(owner.pid, 0);
    return true;
  } catch (error) {
    const code = (error as NodeJS.ErrnoException).code;
    return code !== "ESRCH";
  }
}

async function fileExists(path: string): Promise<boolean> {
  try {
    await stat(path);
    return true;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return false;
    throw error;
  }
}

async function removePartialFiles(partialPath: string): Promise<void> {
  await rm(partialPath, {force: true});
  await rm(`${partialPath}.compact`, {force: true});
  await rm(`${partialPath}.owner`, {force: true});
}
