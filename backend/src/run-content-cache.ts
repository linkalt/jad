import {createHash, type Hash} from "node:crypto";
import {
  closeSync,
  mkdirSync,
  mkdtempSync,
  openSync,
  rmSync,
  writeSync,
} from "node:fs";
import {readFile} from "node:fs/promises";
import {join} from "node:path";

import type {WebUiMessage, WebUiPart} from "./plugins/web-ui/types.js";
import {fieldsFromParts} from "./plugins/web-ui/shared-web-ui.js";
import type {WebUiMessageAppend} from "./plugins/web-ui/incremental-web-ui.js";

export interface RunContentCacheUpdate {
  metadata: WebUiMessage;
  changedContent: ReadonlySet<number>;
}

interface RunContentCacheOptions {
  /** Profile-scoped staging directory; omitted in unit callers that do not spill. */
  directory?: string;
  /** UTF-8 content-byte budget kept in the run snapshot's memory cache. */
  memoryBytes: number;
}

type ContentField = "text" | "arguments" | "output";

interface DiskPart {
  field: ContentField;
  type: WebUiPart["type"];
  path?: string;
  fd?: number;
  utf8Bytes: number;
  storageBytes: number;
  codeUnits: number;
  lastCodeUnit: number;
  digest: string;
  hash: Hash;
  generation: number;
}

/**
 * Keeps the latest Web-UI run message in memory up to a UTF-8 byte budget, then
 * stores only part content in a temporary profile-scoped cache. Message shape
 * and part metadata remain in memory; the cache is never transcript state.
 */
export class RunContentCache {
  private memoryMessage: WebUiMessage | undefined;
  private diskMessage: WebUiMessage | undefined;
  private readonly diskParts = new Map<number, DiskPart>();
  private readonly openFileDescriptors = new Set<number>();
  private fileSequence = 0;
  private cacheDirectory: string | undefined;
  private spilled = false;
  private disabled = false;
  private activeReads = 0;
  private deleteAfterReads = false;
  private readonly digestCache = new WeakMap<WebUiPart, string>();

  public constructor(private readonly options: RunContentCacheOptions) {}

  public get isSpilled(): boolean {
    return this.spilled;
  }

  /** Latest state metadata, with content blanked after spill. */
  public metadata(): WebUiMessage | undefined {
    return this.spilled ? this.diskMessage : this.memoryMessage;
  }

  /** Update the message snapshot and persist only the changed content when spilled. */
  public update(message: WebUiMessage, append?: WebUiMessageAppend): RunContentCacheUpdate {
    if (this.disabled || this.options.directory === undefined) {
      this.memoryMessage = message;
      this.diskMessage = undefined;
      this.spilled = false;
      return {metadata: message, changedContent: new Set()};
    }

    try {
      if (!this.spilled) {
        if (messageContentBytes(message) <= this.options.memoryBytes) {
          this.memoryMessage = message;
          return {metadata: message, changedContent: new Set()};
        }
        this.startSpill(message, append?.index);
        return {
          metadata: this.diskMessage!,
          changedContent: new Set(message.parts.map((_part, index) => index)),
        };
      }
      const changedContent = this.updateSpilled(message, append);
      return {metadata: this.diskMessage!, changedContent};
    } catch (error) {
      // A cache failure must not turn a provider stream into a failed turn. Keep
      // the newest complete snapshot in memory as the availability fallback.
      this.disableAfterError(error, message);
      return {
        metadata: message,
        changedContent: new Set(message.parts.map((_part, index) => index)),
      };
    }
  }

  /** Replace only top-level metadata such as the pending interaction. */
  public setMetadata(message: WebUiMessage): void {
    if (this.spilled) {
      this.diskMessage = message;
    } else {
      this.memoryMessage = message;
    }
  }

  /** Materialize a stable snapshot for a subscriber that attaches mid-run. */
  public async read(): Promise<WebUiMessage | undefined> {
    if (!this.spilled || this.diskMessage === undefined) {
      return this.memoryMessage;
    }

    // Capture the small manifest before the first await. Part files are append
    // only, so later chunks cannot change the bytes referenced by this snapshot.
    const manifest = cloneMessage(this.diskMessage);
    const parts = new Map<number, {field: ContentField; path?: string; storageBytes: number}>();
    for (const [index, value] of this.diskParts) {
      parts.set(index, {
        field: value.field,
        ...(value.path === undefined ? {} : {path: value.path}),
        storageBytes: value.storageBytes,
      });
    }

    this.activeReads += 1;
    try {
      const loaded = await Promise.all([...parts].map(async ([index, reference]) => {
        if (reference.path === undefined || reference.storageBytes === 0) {
          return [index, ""] as const;
        }
        const data = await readFile(reference.path);
        if (data.byteLength < reference.storageBytes) {
          throw new Error("incomplete live-run cache content");
        }
        return [
          index,
          data.subarray(0, reference.storageBytes).toString("utf16le"),
        ] as const;
      }));
      const contentByPart = new Map(loaded);
      const restoredParts = manifest.parts.map((part, index): WebUiPart => {
        const reference = parts.get(index);
        const value = contentByPart.get(index) ?? "";
        if (reference === undefined || reference.field === "text") {
          return part.type === "text" || part.type === "reasoning"
            ? {...part, text: value}
            : part;
        }
        if (reference.field === "arguments" && part.type === "tool_call") {
          return {...part, arguments: value};
        }
        if (reference.field === "output" && part.type === "tool_result") {
          return {...part, output: value};
        }
        return part;
      });
      return {
        ...manifest,
        parts: restoredParts,
        ...fieldsFromParts(restoredParts),
      };
    } finally {
      this.activeReads -= 1;
      if (this.activeReads === 0 && this.deleteAfterReads) {
        this.removeCacheDirectory();
      }
    }
  }

  /** Remove the ephemeral file cache when the run is no longer attachable. */
  public dispose(): void {
    this.removeCacheDirectory();
    this.diskParts.clear();
    this.diskMessage = undefined;
    this.memoryMessage = undefined;
    this.spilled = false;
    this.disabled = true;
  }

  private startSpill(message: WebUiMessage, activePartIndex?: number): void {
    mkdirSync(this.options.directory!, {recursive: true});
    this.cacheDirectory = mkdtempSync(join(this.options.directory!, "run-"));
    this.spilled = true;
    this.diskParts.clear();
    message.parts.forEach((part, index) => {
      const content = partContent(part);
      if (content !== undefined) {
        this.diskParts.set(index, this.writeReplacement(index, part, content.value));
      }
    });
    this.diskMessage = stripContent(message);
    this.memoryMessage = undefined;
    this.closeInactiveFileDescriptors(activePartIndex);
  }

  private updateSpilled(message: WebUiMessage, append?: WebUiMessageAppend): Set<number> {
    const nextParts = new Map<number, DiskPart>();
    const changedContent = new Set<number>();
    message.parts.forEach((part, index) => {
      const content = partContent(part);
      if (content === undefined) {
        return;
      }
      const prior = this.diskParts.get(index);
      if (prior === undefined || prior.field !== content.field || prior.type !== part.type) {
        nextParts.set(index, this.writeReplacement(index, part, content.value));
        if (content.value.length > 0) {
          changedContent.add(index);
        }
        return;
      }

      const suppliedAppend = append?.index === index ? append : undefined;
      if (
        suppliedAppend !== undefined &&
        suppliedAppend.append.length > 0 &&
        prior.codeUnits + suppliedAppend.append.length === content.value.length
      ) {
        const bytes = Buffer.byteLength(suppliedAppend.append, "utf8");
        const joinsSurrogatePair = prior.lastCodeUnit >= 0xd800 &&
          prior.lastCodeUnit <= 0xdbff &&
          suppliedAppend.append.charCodeAt(0) >= 0xdc00 &&
          suppliedAppend.append.charCodeAt(0) <= 0xdfff;
        const hash = prior.hash.copy().update(suppliedAppend.append, "utf16le");
        const digest = hash.copy().digest("hex");
        let path = prior.path;
        let fd = prior.fd;
        if (path === undefined) {
          path = this.newPartPath(index, prior.generation);
          fd = openSync(path, "wx");
          this.openFileDescriptors.add(fd);
        } else if (fd === undefined) {
          fd = openSync(path, "a");
          this.openFileDescriptors.add(fd);
        }
        writeSync(fd, suppliedAppend.append, null, "utf16le");
        nextParts.set(index, {
          ...prior,
          path,
          fd,
          utf8Bytes: prior.utf8Bytes + bytes - (joinsSurrogatePair ? 2 : 0),
          storageBytes: prior.storageBytes + suppliedAppend.append.length * 2,
          codeUnits: content.value.length,
          lastCodeUnit: suppliedAppend.append.charCodeAt(suppliedAppend.append.length - 1),
          digest,
          hash,
        });
        changedContent.add(index);
        return;
      }

      const digest = this.digest(part, content.value);
      if (prior.codeUnits === content.value.length && prior.digest === digest) {
        nextParts.set(index, prior);
      } else {
        nextParts.set(index, this.writeReplacement(index, part, content.value, digest));
        changedContent.add(index);
      }
    });

    for (const [index, previous] of this.diskParts) {
      const next = nextParts.get(index);
      if (previous.fd !== undefined && next?.fd !== previous.fd) {
        this.closeFileDescriptor(previous.fd);
      }
    }
    this.diskParts.clear();
    for (const [index, part] of nextParts) {
      this.diskParts.set(index, part);
    }
    this.diskMessage = stripContent(message);
    this.closeInactiveFileDescriptors(append?.index);
    return changedContent;
  }

  private writeReplacement(
    index: number,
    part: WebUiPart,
    value: string,
    knownDigest?: string,
  ): DiskPart {
    const field = partContent(part)!.field;
    const generation = (this.diskParts.get(index)?.generation ?? -1) + 1;
    const path = value.length === 0 ? undefined : this.newPartPath(index, generation);
    let fd: number | undefined;
    if (path !== undefined) {
      fd = openSync(path, "wx");
      this.openFileDescriptors.add(fd);
      writeSync(fd, value, null, "utf16le");
    }
    const hash = createHash("sha256").update(value, "utf16le");
    return {
      field,
      type: part.type,
      ...(path === undefined ? {} : {path, fd}),
      utf8Bytes: Buffer.byteLength(value, "utf8"),
      storageBytes: value.length * 2,
      codeUnits: value.length,
      lastCodeUnit: value.length === 0 ? -1 : value.charCodeAt(value.length - 1),
      digest: knownDigest ?? hash.copy().digest("hex"),
      hash,
      generation,
    };
  }

  private newPartPath(index: number, generation: number): string {
    const sequence = this.fileSequence++;
    return join(this.cacheDirectory!, `part-${index}-${generation}-${sequence}.bin`);
  }

  private closeFileDescriptor(fd: number): void {
    if (!this.openFileDescriptors.delete(fd)) {
      return;
    }
    try {
      closeSync(fd);
    } catch (error) {
      console.warn(`[web-ui-run-cache] failed to close cache file: ${error instanceof Error ? error.message : String(error)}`);
    }
  }

  private closeInactiveFileDescriptors(activePartIndex?: number): void {
    for (const [index, part] of this.diskParts) {
      if (index !== activePartIndex && part.fd !== undefined) {
        this.closeFileDescriptor(part.fd);
        const {fd: _fd, ...withoutDescriptor} = part;
        this.diskParts.set(index, withoutDescriptor);
      }
    }
  }

  private closeOpenFileDescriptors(): void {
    for (const fd of [...this.openFileDescriptors]) {
      this.closeFileDescriptor(fd);
    }
  }

  private digest(part: WebUiPart, value: string): string {
    const cached = this.digestCache.get(part);
    if (cached !== undefined) {
      return cached;
    }
    const digest = createHash("sha256").update(value, "utf16le").digest("hex");
    this.digestCache.set(part, digest);
    return digest;
  }

  private disableAfterError(error: unknown, newest: WebUiMessage): void {
    console.warn(`[web-ui-run-cache] spill disabled: ${error instanceof Error ? error.message : String(error)}`);
    this.removeCacheDirectory();
    this.diskParts.clear();
    this.diskMessage = undefined;
    this.memoryMessage = newest;
    this.spilled = false;
    this.disabled = true;
  }

  private removeCacheDirectory(): void {
    if (this.cacheDirectory === undefined) {
      return;
    }
    this.closeOpenFileDescriptors();
    if (this.activeReads > 0) {
      this.deleteAfterReads = true;
      return;
    }
    rmSync(this.cacheDirectory, {recursive: true, force: true});
    this.cacheDirectory = undefined;
    this.deleteAfterReads = false;
  }
}

function partContent(part: WebUiPart): {field: ContentField; value: string} | undefined {
  switch (part.type) {
    case "text":
    case "reasoning":
      return {field: "text", value: part.text};
    case "tool_call":
      return {field: "arguments", value: part.arguments};
    case "tool_result":
      return {field: "output", value: part.output};
  }
}

function messageContentBytes(message: WebUiMessage): number {
  let bytes = 0;
  for (const part of message.parts) {
    const content = partContent(part);
    if (content !== undefined) {
      bytes += Buffer.byteLength(content.value, "utf8");
    }
  }
  return bytes;
}

function stripContent(message: WebUiMessage): WebUiMessage {
  const parts = message.parts.map((part): WebUiPart => {
    switch (part.type) {
      case "text":
      case "reasoning":
        return {...part, text: ""};
      case "tool_call":
        return {...part, arguments: ""};
      case "tool_result":
        return {...part, output: ""};
    }
  });
  const {reasoningContent: _reasoningContent, ...rest} = message;
  return {...rest, text: "", parts};
}

function cloneMessage(message: WebUiMessage): WebUiMessage {
  return {
    ...message,
    parts: message.parts.map((part) => ({...part})),
    ...(message.steps === undefined ? {} : {steps: message.steps.map((step) => ({...step}))}),
  };
}
