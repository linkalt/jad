import {randomUUID} from "node:crypto";
import {mkdir, readdir, readFile, rename, stat, unlink, writeFile} from "node:fs/promises";
import {join} from "node:path";

import type {NativeFetchToolSettings} from "../../config.js";
import {AsyncMutex} from "../../concurrency.js";

export interface FetchDocument {
  id: string;
  url: string;
  finalUrl: string;
  title?: string;
  contentType: string;
  status: number;
  fetchedAt: string;
  markdown: string;
}

interface StoredFetchMetadata extends Omit<FetchDocument, "markdown"> {}

export class FetchStoreError extends Error {
  public constructor(message: string) {
    super(message);
    this.name = "FetchStoreError";
  }
}

function cursorFor(id: string, offset: number): string {
  return `${id}.${offset}`;
}

function parseCursor(cursor: string): {id: string; offset: number} {
  const match = /^([0-9a-f]{8}-(?:[0-9a-f]{4}-){3}[0-9a-f]{12})\.([0-9]+)$/i.exec(cursor);
  const id = match?.[1];
  const offsetText = match?.[2];
  if (!id || !offsetText) throw new FetchStoreError("fetch cursor is invalid");
  const offset = Number(offsetText);
  if (!Number.isSafeInteger(offset)) throw new FetchStoreError("fetch cursor is invalid");
  return {id: id.toLowerCase(), offset};
}

async function directoryBytes(directory: string): Promise<number> {
  try {
    const entries = await readdir(directory, {withFileTypes: true});
    const sizes = await Promise.all(entries.filter((entry) => entry.isFile()).map(async (entry) => (await stat(join(directory, entry.name))).size));
    return sizes.reduce((total, size) => total + size, 0);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return 0;
    throw error;
  }
}

async function atomicWrite(path: string, contents: string): Promise<void> {
  const temporary = `${path}.${randomUUID()}.tmp`;
  await writeFile(temporary, contents, "utf8");
  await rename(temporary, path);
}

export class FetchStore {
  /** Serializes the capacity check with the write, so concurrent fetches cannot overshoot it. */
  private readonly writeMutex = new AsyncMutex();

  public constructor(private readonly directory: string) {}

  public async create(document: Omit<FetchDocument, "id" | "fetchedAt">, policy: NativeFetchToolSettings): Promise<FetchDocument> {
    return this.writeMutex.runExclusive(() => this.createUnlocked(document, policy));
  }

  private async createUnlocked(document: Omit<FetchDocument, "id" | "fetchedAt">, policy: NativeFetchToolSettings): Promise<FetchDocument> {
    const id = randomUUID();
    const fetchedAt = new Date().toISOString();
    const stored: FetchDocument = {...document, id, fetchedAt};
    const metadata: StoredFetchMetadata = {
      id, url: stored.url, finalUrl: stored.finalUrl, ...(stored.title ? {title: stored.title} : {}),
      contentType: stored.contentType, status: stored.status, fetchedAt,
    };
    const metadataText = JSON.stringify(metadata);
    const markdownBytes = Buffer.byteLength(stored.markdown, "utf8");
    const storedBytes = markdownBytes + Buffer.byteLength(metadataText, "utf8");
    if (storedBytes > policy.max_storage_bytes) throw new FetchStoreError("fetched document exceeds configured storage capacity");
    await mkdir(this.directory, {recursive: true});
    if ((await directoryBytes(this.directory)) + storedBytes > policy.max_storage_bytes) {
      throw new FetchStoreError("fetch storage is full");
    }
    try {
      await atomicWrite(join(this.directory, `${id}.md`), stored.markdown);
      await atomicWrite(join(this.directory, `${id}.json`), metadataText);
    } catch (error) {
      await unlink(join(this.directory, `${id}.md`)).catch(() => undefined);
      throw error;
    }
    return stored;
  }

  public async page(cursor: string, limit: number): Promise<{document: FetchDocument; content: string; nextCursor?: string}> {
    const {id, offset} = parseCursor(cursor);
    let metadata: StoredFetchMetadata;
    let markdown: string;
    try {
      [metadata, markdown] = await Promise.all([
        readFile(join(this.directory, `${id}.json`), "utf8").then((text) => JSON.parse(text) as StoredFetchMetadata),
        readFile(join(this.directory, `${id}.md`), "utf8"),
      ]);
    } catch {
      throw new FetchStoreError("fetch cursor was not found");
    }
    if (metadata.id !== id || offset > markdown.length) throw new FetchStoreError("fetch cursor is invalid");
    const content = markdown.slice(offset, offset + limit);
    const nextOffset = offset + content.length;
    return {document: {...metadata, markdown}, content, ...(nextOffset < markdown.length ? {nextCursor: cursorFor(id, nextOffset)} : {})};
  }

  public async createPage(document: Omit<FetchDocument, "id" | "fetchedAt">, limit: number, policy: NativeFetchToolSettings): Promise<{document: FetchDocument; content: string; nextCursor?: string}> {
    const stored = await this.create(document, policy);
    const content = stored.markdown.slice(0, limit);
    return {document: stored, content, ...(content.length < stored.markdown.length ? {nextCursor: cursorFor(stored.id, content.length)} : {})};
  }
}
