import {createHash, randomUUID} from "node:crypto";
import {lstat, mkdir, open, readdir, readFile, rename, stat, unlink} from "node:fs/promises";
import {join} from "node:path";

import {AsyncMutex} from "../../concurrency.js";
import type {NativeMemoryToolSettings} from "../../config.js";
import type {JsonRecord} from "../../persistence.js";

const TYPE = /^[a-z][a-z0-9_-]{0,63}$/;
const SEGMENT = /^(?!__proto__$|prototype$|constructor$)[A-Za-z_$][A-Za-z0-9_$]*$/;

export class MemoryStoreError extends Error { public override name = "MemoryStoreError"; }
export class MemoryConflictError extends MemoryStoreError {
  public override name = "MemoryConflictError";
  public constructor(public readonly currentEtag: string) {
    super("memory record changed; retrieve it before updating");
  }
}
export interface MemoryItem { ref: string; etag: string; data: JsonRecord; }
export interface MemoryListResult { items: MemoryItem[]; nextCursor?: string; warnings: string[]; hasMore: boolean; }
interface Totals { types: number; records: number; bytes: number; typeRecords: number; }

function etag(text: string): string { return `sha256:${createHash("sha256").update(text).digest("hex")}`; }
function serialize(data: JsonRecord): string { return `${JSON.stringify(data, null, 2)}\n`; }
function filename(type: string, sequence: number, now: Date): string { return `${type}-${now.toISOString().replace(/[-:]/g, "").replace(/\.(\d{3})Z$/, ".$1Z")}-${String(sequence).padStart(16, "0")}-${randomUUID()}.json`; }
function validFilename(type: string, value: string): boolean { return new RegExp(`^${type.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}-\\d{8}T\\d{6}\\.\\d{3}Z-(?:\\d{16}-)?[0-9a-f-]{36}\\.json$`).test(value); }
interface LegacyCursor { v: 1 | 2; type: string; order: "newest" | "oldest"; filter: string; last: string; }
interface StoredCursor { v: 1; type: string; order: "newest" | "oldest"; filter: JsonRecord; filterFingerprint: string; last: string; }
function legacyCursor(value: unknown): LegacyCursor | undefined {
  if (typeof value !== "string" || value.length > 2048) return undefined;
  try { const parsed = JSON.parse(Buffer.from(value, "base64url").toString("utf8")); return (parsed?.v === 1 || parsed?.v === 2) && typeof parsed.type === "string" && (parsed.order === "newest" || parsed.order === "oldest") && typeof parsed.filter === "string" && typeof parsed.last === "string" ? parsed : undefined; } catch { return undefined; }
}
function cursorId(value: unknown): string | undefined { const match = typeof value === "string" ? /^m_([0-9a-f]{8}-(?:[0-9a-f]{4}-){3}[0-9a-f]{12})$/i.exec(value) : undefined; return match?.[1]?.toLowerCase(); }
function record(value: unknown): value is JsonRecord { return typeof value === "object" && value !== null && !Array.isArray(value); }
function canonical(value: unknown): string { if (Array.isArray(value)) return `[${value.map(canonical).join(",")}]`; if (typeof value === "object" && value !== null) return `{${Object.keys(value).sort().map((key) => `${JSON.stringify(key)}:${canonical((value as JsonRecord)[key])}`).join(",")}}`; return JSON.stringify(value); }
function fingerprint(filter: JsonRecord): string { return createHash("sha256").update(canonical(filter)).digest("hex"); }
function valueAt(data: JsonRecord, path: string): unknown { let value: unknown = data; for (const part of path.split(".")) { if (!SEGMENT.test(part) || typeof value !== "object" || value === null || Array.isArray(value) || !(part in value)) return undefined; value = (value as JsonRecord)[part]; } return value; }
function equal(left: unknown, right: unknown): boolean { return canonical(left) === canonical(right); }
function matches(data: JsonRecord, filter: JsonRecord): boolean { return Object.entries(filter).every(([key, value]) => valueAt(data, key) !== undefined && equal(valueAt(data, key), value)); }
function depth(value: unknown): number { if (typeof value !== "object" || value === null) return 0; return 1 + Math.max(0, ...Object.values(value).map(depth)); }

export class MemoryStore {
  private readonly mutex = new AsyncMutex();
  public constructor(private readonly root: string, private readonly now: () => Date = () => new Date()) {}
  public static validType(type: unknown): type is string { return typeof type === "string" && TYPE.test(type); }

  private async ensureRoot(): Promise<void> { await mkdir(this.root, {recursive: true}); const entry = await lstat(this.root); if (!entry.isDirectory() || entry.isSymbolicLink()) throw new MemoryStoreError("memory storage path is not a safe directory"); }
  private async typeDirectory(type: string, create: boolean): Promise<string | undefined> { const path = join(this.root, type); try { const entry = await lstat(path); if (!entry.isDirectory() || entry.isSymbolicLink()) throw new MemoryStoreError("memory type storage is not a safe directory"); return path; } catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error; if (!create) return undefined; await mkdir(path, {recursive: false}); const entry = await lstat(path); if (!entry.isDirectory() || entry.isSymbolicLink()) throw new MemoryStoreError("memory type storage is not a safe directory"); return path; } }
  private async files(type: string): Promise<string[]> { const path = await this.typeDirectory(type, false); return path ? (await readdir(path, {withFileTypes: true})).filter((entry) => entry.isFile() && validFilename(type, entry.name)).map((entry) => entry.name) : []; }
  private async totals(type: string, policy: NativeMemoryToolSettings): Promise<Totals> {
    await this.ensureRoot(); const dirs = await readdir(this.root, {withFileTypes: true}); let types = 0, records = 0, bytes = 0, typeRecords = 0;
    for (const dir of dirs) { if (!dir.isDirectory() || !TYPE.test(dir.name)) continue; types++; for (const name of await this.files(dir.name)) { records++; const size = (await stat(join(this.root, dir.name, name))).size; bytes += size; if (dir.name === type) typeRecords++; if (records > policy.max_scan_records || bytes > policy.max_scan_bytes) throw new MemoryStoreError("memory quota scan exceeded its configured budget"); } }
    return {types, records, bytes, typeRecords};
  }
  private checkType(type: string, policy: NativeMemoryToolSettings): void { if (!MemoryStore.validType(type)) throw new MemoryStoreError("memory type is invalid"); if (policy.allowed_types && !policy.allowed_types.includes(type)) throw new MemoryStoreError("memory type is not allowed"); }
  private checkData(data: unknown, policy: NativeMemoryToolSettings): asserts data is JsonRecord { if (typeof data !== "object" || data === null || Array.isArray(data) || depth(data) > policy.max_json_depth) throw new MemoryStoreError("memory data must be a JSON object within the configured depth"); if (Buffer.byteLength(serialize(data as JsonRecord), "utf8") > policy.max_record_bytes) throw new MemoryStoreError("memory data exceeds the configured record size"); }
  private target(type: string, ref: string): string { const parts = ref.split("/"); const name = parts[1]; if (parts.length !== 2 || parts[0] !== type || typeof name !== "string" || !validFilename(type, name)) throw new MemoryStoreError("memory ref is invalid for the selected type"); return join(this.root, type, name); }
  private async load(type: string, ref: string): Promise<{path: string; text: string; data: JsonRecord}> { const path = this.target(type, ref); let text: string; try { const entry = await lstat(path); if (!entry.isFile() || entry.isSymbolicLink()) throw new MemoryStoreError("memory record is not a safe file"); text = await readFile(path, "utf8"); } catch (error) { if ((error as NodeJS.ErrnoException).code === "ENOENT") throw new MemoryStoreError("memory record was not found"); throw error; } try { const data: unknown = JSON.parse(text); if (typeof data !== "object" || data === null || Array.isArray(data)) throw new Error(); return {path, text, data: data as JsonRecord}; } catch { throw new MemoryStoreError("memory record is malformed"); } }
  private async write(path: string, text: string): Promise<void> { const temporary = `${path}.${randomUUID()}.tmp`; await mkdir(join(path, ".."), {recursive: true}); const handle = await open(temporary, "wx", 0o600); try { await handle.writeFile(text, "utf8"); } finally { await handle.close(); } try { await rename(temporary, path); } catch (error) { await unlink(temporary).catch(() => undefined); throw error; } }
  private async cursorDirectory(create: boolean): Promise<string | undefined> {
    const path = join(this.root, ".memory-cursors");
    try {
      const entry = await lstat(path);
      if (!entry.isDirectory() || entry.isSymbolicLink()) throw new MemoryStoreError("memory cursor storage is not a safe directory");
      return path;
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
      if (!create) return undefined;
      await mkdir(path, {recursive: false});
      const entry = await lstat(path);
      if (!entry.isDirectory() || entry.isSymbolicLink()) throw new MemoryStoreError("memory cursor storage is not a safe directory");
      return path;
    }
  }
  private async pruneCursors(directory: string, policy: NativeMemoryToolSettings): Promise<void> {
    const now = Date.now();
    const active: Array<{name: string; mtimeMs: number}> = [];
    for (const entry of await readdir(directory, {withFileTypes: true})) {
      if (!entry.isFile() || !/^[0-9a-f-]{36}\.json$/i.test(entry.name)) continue;
      const info = await stat(join(directory, entry.name));
      if (now - info.mtimeMs >= policy.cursor_ttl_ms) await unlink(join(directory, entry.name));
      else active.push({name: entry.name, mtimeMs: info.mtimeMs});
    }
    active.sort((left, right) => left.mtimeMs - right.mtimeMs);
    for (const entry of active.slice(Math.max(0, policy.max_cursors - 1))) await unlink(join(directory, entry.name));
  }
  private async readStoredCursor(value: unknown, policy: NativeMemoryToolSettings): Promise<StoredCursor | undefined> {
    const id = cursorId(value);
    if (!id) return undefined;
    const directory = await this.cursorDirectory(false);
    if (!directory) throw new MemoryStoreError("memory cursor was not found");
    const path = join(directory, `${id}.json`);
    let text: string;
    try {
      const info = await lstat(path);
      if (!info.isFile() || info.isSymbolicLink()) throw new MemoryStoreError("memory cursor is not a safe file");
      if (Date.now() - info.mtimeMs >= policy.cursor_ttl_ms) { await unlink(path); throw new MemoryStoreError("memory cursor expired"); }
      text = await readFile(path, "utf8");
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "ENOENT") throw new MemoryStoreError("memory cursor was not found");
      throw error;
    }
    try {
      const parsed: unknown = JSON.parse(text);
      if (!record(parsed) || parsed.v !== 1 || typeof parsed.type !== "string" || (parsed.order !== "newest" && parsed.order !== "oldest") || !record(parsed.filter) || typeof parsed.filterFingerprint !== "string" || typeof parsed.last !== "string" || fingerprint(parsed.filter) !== parsed.filterFingerprint) throw new Error();
      return parsed as unknown as StoredCursor;
    } catch { throw new MemoryStoreError("memory cursor is invalid"); }
  }
  private async createCursor(type: string, order: "newest" | "oldest", filter: JsonRecord, filterFingerprint: string, last: string, policy: NativeMemoryToolSettings): Promise<string> {
    const stored: StoredCursor = {v: 1, type, order, filter, filterFingerprint, last};
    const text = JSON.stringify(stored);
    if (Buffer.byteLength(text, "utf8") > policy.max_cursor_bytes) throw new MemoryStoreError("memory cursor exceeds configured size");
    const directory = await this.cursorDirectory(true);
    await this.pruneCursors(directory!, policy);
    const id = randomUUID();
    await this.write(join(directory!, `${id}.json`), text);
    return `m_${id}`;
  }
  private async nextSequence(): Promise<number> {
    const path = join(this.root, ".memory-sequence");
    let current = 0;
    try {
      const text = await readFile(path, "utf8");
      if (!/^\d+\n?$/.test(text)) throw new MemoryStoreError("memory sequence is malformed");
      current = Number(text.trim());
      if (!Number.isSafeInteger(current) || current < 0) throw new MemoryStoreError("memory sequence is malformed");
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
    }
    if (current >= Number.MAX_SAFE_INTEGER) throw new MemoryStoreError("memory sequence is exhausted");
    const next = current + 1;
    await this.write(path, `${next}\n`);
    return next;
  }

  public async create(type: string, data: unknown, policy: NativeMemoryToolSettings): Promise<MemoryItem> { return this.mutex.runExclusive(async () => { this.checkType(type, policy); this.checkData(data, policy); const total = await this.totals(type, policy); const text = serialize(data); if (total.typeRecords >= policy.max_records_per_type || total.records >= policy.max_total_records || total.bytes + Buffer.byteLength(text) > policy.max_total_bytes || (total.typeRecords === 0 && total.types >= policy.max_types)) throw new MemoryStoreError("memory capacity has been reached"); const name = filename(type, await this.nextSequence(), this.now()); await this.typeDirectory(type, true); await this.write(join(this.root, type, name), text); return {ref: `${type}/${name}`, etag: etag(text), data}; }); }
  public async get(type: string, ref: string, policy: NativeMemoryToolSettings): Promise<MemoryItem> { return this.mutex.runExclusive(async () => { this.checkType(type, policy); const record = await this.load(type, ref); return {ref, etag: etag(record.text), data: record.data}; }); }
  public async update(type: string, ref: string, ifMatch: string | undefined, data: unknown, policy: NativeMemoryToolSettings): Promise<MemoryItem> { return this.mutex.runExclusive(async () => { this.checkType(type, policy); this.checkData(data, policy); const current = await this.load(type, ref); const currentEtag = etag(current.text); if (ifMatch !== undefined && currentEtag !== ifMatch) throw new MemoryConflictError(currentEtag); const next = serialize(data); const total = await this.totals(type, policy); if (total.bytes - Buffer.byteLength(current.text) + Buffer.byteLength(next) > policy.max_total_bytes) throw new MemoryStoreError("memory capacity has been reached"); await this.write(current.path, next); return {ref, etag: etag(next), data}; }); }
  public async list(type: string, requestedFilter: JsonRecord | undefined, limit: number, requestedOrder: unknown, token: unknown, policy: NativeMemoryToolSettings, options: {createCursor?: boolean} = {}): Promise<MemoryListResult> { return this.mutex.runExclusive(async () => {
    this.checkType(type, policy);
    const legacy = token === undefined ? undefined : legacyCursor(token);
    const stored = token === undefined || legacy ? undefined : await this.readStoredCursor(token, policy);
    if (token !== undefined && !legacy && !stored) throw new MemoryStoreError("memory cursor is invalid");
    const filter = requestedFilter ?? stored?.filter ?? {};
    if (Object.keys(filter).length > policy.max_filter_fields || Object.keys(filter).some((key) => key.split(".").length > policy.max_filter_depth || key.split(".").some((part) => !SEGMENT.test(part)))) throw new MemoryStoreError("memory filter exceeds configured limits");
    const suppliedOrder = requestedOrder === undefined ? undefined : requestedOrder === "newest" || requestedOrder === "oldest" ? requestedOrder : undefined;
    if (requestedOrder !== undefined && !suppliedOrder) throw new MemoryStoreError("memory order is invalid");
    const order = suppliedOrder ?? legacy?.order ?? stored?.order ?? "newest";
    const fp = fingerprint(filter);
    if (legacy && (legacy.type !== type || legacy.order !== order || legacy.filter !== fp)) throw new MemoryStoreError("memory cursor is invalid");
    if (stored && (stored.type !== type || stored.order !== order || stored.filterFingerprint !== fp)) throw new MemoryStoreError("memory cursor is invalid");
    const last = legacy?.last ?? stored?.last;
    const names = (await this.files(type)).sort((a, b) => order === "newest" ? b.localeCompare(a) : a.localeCompare(b));
    const positionIndex = last ? names.indexOf(last.slice(type.length + 1)) : -1;
    if (last && positionIndex === -1) throw new MemoryStoreError("memory cursor position was not found");
    // Legacy v1 described the first unreturned item; persistent cursors describe the last returned item.
    const start = last ? positionIndex - (legacy?.v === 1 ? 1 : 0) : -1;
    const items: MemoryItem[] = [], warnings: string[] = [];
    let scanned = 0, scannedBytes = 0, position: string | undefined, stopped = false;
    for (const name of names.slice(start + 1)) {
      if (scanned >= policy.max_scan_records) { stopped = true; break; }
      const ref = `${type}/${name}`;
      scanned += 1;
      try {
        const text = await readFile(join(this.root, type, name), "utf8");
        if (scannedBytes + Buffer.byteLength(text) > policy.max_scan_bytes) { stopped = true; break; }
        scannedBytes += Buffer.byteLength(text);
        const data: unknown = JSON.parse(text);
        if (!record(data)) throw new Error();
        if (!matches(data, filter)) { position = ref; continue; }
        const candidate = {ref, etag: etag(text), data};
        if (items.length >= limit || Buffer.byteLength(JSON.stringify([...items, candidate]), "utf8") > policy.max_list_bytes) { stopped = true; break; }
        items.push(candidate);
        position = ref;
      } catch {
        position = ref;
        if (warnings.length < 8) warnings.push(`unreadable record: ${ref}`);
      }
    }
    const nextCursor = stopped && position && options.createCursor !== false ? await this.createCursor(type, order, filter, fp, position, policy) : undefined;
    return {items, warnings, hasMore: stopped, ...(nextCursor ? {nextCursor} : {})};
  }); }
}
