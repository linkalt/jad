import {join} from "node:path";

import {ResponsePartialWriter} from "./response-partial-writer.js";
import {RESPONSE_PARTIAL_FILE} from "./response-partial-files.js";
import {DEFAULT_ROOT_SERVER_SETTINGS} from "./session-roots.js";

import type {WebUiMessage, WebUiPart} from "./plugins/web-ui/types.js";
import {fieldsFromParts} from "./plugins/web-ui/shared-web-ui.js";
import type {WebUiMessageAppend} from "./plugins/web-ui/incremental-web-ui.js";

export interface RunContentCacheUpdate {
  metadata: WebUiMessage;
  changedContent: ReadonlySet<number>;
  /** Resolves when the cache has admitted this update within its write budget. */
  capacity: Promise<void>;
}

export interface RunContentCacheStagedReader {
  read: () => Promise<string>;
  codeUnits: number;
}

interface RunContentCacheOptions {
  /** Profile-scoped fallback staging root when no response-turn path is supplied. */
  directory?: string;
  /** UTF-8 bytes of active-response content retained before spilling. */
  memoryBytes: number;
  /** Target UTF-8 bytes per asynchronous append batch. */
  writeBatchBytes?: number;
  /** Maximum delay before a buffered batch is flushed. */
  flushIntervalMs?: number;
  /** Maximum redundant item ranges retained before compacting the shared file. */
  maxIndexSegments?: number;
}

type ContentField = "text" | "arguments" | "output";

interface StagedContentSource {
  responseKey: string;
  outputIndex: number;
  codeUnits: number;
  read: () => Promise<string>;
}

interface ContentContribution {
  itemKey: string;
  /** UTF-16 code-unit offset within the output item's content. */
  offset: number;
  codeUnits: number;
}

interface PartContentState {
  field: ContentField;
  type: WebUiPart["type"];
  baseValue: string;
  includeBase: boolean;
  /** Ordered coalesced runs of output-item content in the normalized part. */
  contributions: ContentContribution[];
  stagedSources: Map<string, StagedContentSource>;
}

interface ActiveContentSource {
  key: string;
  responseKey: string;
  outputIndex: number | undefined;
  partIndex: number;
  content: string;
  codeUnits: number;
  memoryBytes: number;
}

/**
 * Keeps a normalized message scheme in memory and content deltas for any
 * concurrently active output items. Spilled bytes are raw UTF-16LE content in
 * one responses.partial file; item ranges and per-part ordering stay in memory.
 */
export class RunContentCache {
  private memoryMessage: WebUiMessage | undefined;
  private diskMessage: WebUiMessage | undefined;
  private baselineMessage: WebUiMessage | undefined;
  private latestShape: WebUiMessage | undefined;
  private readonly partContents = new Map<number, PartContentState>();
  private readonly activeItems = new Map<string, ActiveContentSource>();
  private readonly activeReads = new Set<Promise<WebUiMessage | undefined>>();
  private currentResponseKey: string | undefined;
  private partialDirectory: string | undefined;
  private partialFilePath: string | undefined;
  private partialWriter: ResponsePartialWriter | undefined;
  private partialCleanupPromise: Promise<void> | undefined;
  private fallbackMessageProvider: (() => WebUiMessage | undefined) | undefined;
  private activeMemoryBytes = 0;
  private nextLegacySourceId = 0;
  private spilled = false;
  private disabled = false;
  private readonly writeBatchBytes: number;
  private readonly flushIntervalMs: number;
  private readonly maxIndexSegments: number;

  public constructor(private readonly options: RunContentCacheOptions) {
    this.writeBatchBytes = options.writeBatchBytes ?? DEFAULT_ROOT_SERVER_SETTINGS.run_content_write_batch_bytes;
    this.flushIntervalMs = options.flushIntervalMs ?? DEFAULT_ROOT_SERVER_SETTINGS.run_content_flush_interval_ms;
    this.maxIndexSegments = options.maxIndexSegments ?? DEFAULT_ROOT_SERVER_SETTINGS.run_content_max_index_segments;
  }

  public get isSpilled(): boolean {
    return this.spilled;
  }

  /** Latest message or its content-free normalized scheme. */
  public metadata(): WebUiMessage | undefined {
    return this.memoryMessage ?? this.diskMessage ?? this.baselineMessage;
  }

  /** Starts the partial-content lifetime for one Responses turn. */
  public async beginResponse(
    directory: string,
    responseKey: string,
    baselineMessage?: WebUiMessage,
  ): Promise<void> {
    const candidate = baselineMessage ?? this.memoryMessage ?? this.baselineMessage;
    const baseline = candidate !== undefined && candidate.parts.length === 0 && candidate.text === ""
      ? undefined
      : candidate;
    await this.cleanupPartialFile();
    this.currentResponseKey = responseKey;
    this.partialDirectory = directory;
    this.baselineMessage = baseline;
    this.memoryMessage = baseline;
    this.latestShape = baseline;
    this.diskMessage = undefined;
    this.partContents.clear();
    this.clearActiveItems();
    this.nextLegacySourceId = 0;
    this.spilled = false;
  }

  /**
   * Replaces staged output-item deltas with the authoritative persistence
   * reader, then removes those item's bytes from the shared partial file.
   */
  public async handoffOutputItem(
    responseKey: string,
    outputIndex: number,
    readers: ReadonlyMap<number, RunContentCacheStagedReader>,
  ): Promise<void> {
    if (responseKey !== this.currentResponseKey) return;

    const visibleMessage = this.memoryMessage ?? this.latestShape ?? this.baselineMessage;
    const retiringKeys = new Set<string>();
    for (const [partIndex, reader] of readers) {
      const part = visibleMessage?.parts[partIndex] ?? this.baselineMessage?.parts[partIndex];
      if (part === undefined) continue;
      const state = this.partContents.get(partIndex) ?? this.createPartState(partIndex, part);
      this.partContents.set(partIndex, state);
      const key = this.sourceKey(responseKey, outputIndex, partIndex);
      if (!state.stagedSources.has(key)) {
        state.stagedSources.set(key, {
          responseKey,
          outputIndex,
          codeUnits: reader.codeUnits,
          read: reader.read,
        });
      }
      this.reconcileContributionLength(state, key, reader.codeUnits);
      const active = this.activeItems.get(key);
      if (active !== undefined) {
        this.activeMemoryBytes = Math.max(0, this.activeMemoryBytes - active.memoryBytes);
        this.activeItems.delete(key);
      }
      retiringKeys.add(key);
    }

    // Empty output items have no staged content reader, but their pending empty
    // cache entries can still be retired safely.
    for (const [key, active] of this.activeItems) {
      if (active.responseKey === responseKey && active.outputIndex === outputIndex && active.codeUnits === 0) {
        this.activeItems.delete(key);
        retiringKeys.add(key);
      }
    }

    if (retiringKeys.size > 0 && this.partialWriter !== undefined) {
      const reads = [...this.activeReads];
      await Promise.allSettled(reads);
      await this.partialWriter.removeItems(retiringKeys);
    }

    if (this.memoryMessage !== undefined && readers.size > 0) {
      this.latestShape = stripContent(this.memoryMessage);
      this.diskMessage = this.latestShape;
      this.memoryMessage = undefined;
    }
    this.spilled = this.spilled || readers.size > 0;
  }

  /** Installs a complete source before retiring staged readers for this response. */
  public async commitResponse(responseKey: string, message: WebUiMessage): Promise<void> {
    if (responseKey !== this.currentResponseKey) return;

    // Publish the new complete source first so later subscribers cannot acquire
    // readers that depend on staging while existing snapshot reads are drained.
    this.currentResponseKey = undefined;
    this.partialDirectory = undefined;
    this.baselineMessage = message;
    this.memoryMessage = message;
    this.diskMessage = undefined;
    this.latestShape = message;
    this.spilled = false;

    await Promise.allSettled([...this.activeReads]);
    await this.cleanupPartialFile();
    this.partContents.clear();
    this.clearActiveItems();
  }

  /** Update the normalized scheme and append only a supplied content delta. */
  public update(
    message: WebUiMessage,
    append?: WebUiMessageAppend,
    outputIndex?: number,
    partIndex?: number,
  ): RunContentCacheUpdate {
    if (this.disabled || this.currentResponseKey === undefined) {
      this.memoryMessage = message;
      this.baselineMessage = message;
      this.diskMessage = undefined;
      this.latestShape = message;
      this.spilled = false;
      return {metadata: message, changedContent: new Set(), capacity: Promise.resolve()};
    }

    let capacity = Promise.resolve();
    try {
      const responseKey = this.currentResponseKey;
      const sourceIndex = append?.outputIndex ?? outputIndex;
      let index = append?.index ?? partIndex;
      if (index === undefined && sourceIndex !== undefined) {
        index = [...this.activeItems.values()].find(
          (item) => item.responseKey === responseKey && item.outputIndex === sourceIndex,
        )?.partIndex;
      }
      const changedContent = new Set<number>();
      const part = index === undefined ? undefined : message.parts[index];
      const content = partContent(part);
      if (index !== undefined && part !== undefined && content !== undefined) {
        const state = this.partContents.get(index) ?? this.createPartState(index, part);
        this.partContents.set(index, state);
        let active = this.findActiveSource(responseKey, sourceIndex, index);
        const isNew = active === undefined;
        if (active === undefined) {
          const key = sourceIndex === undefined
            ? `${this.sourceKey(responseKey, undefined, index)}\u0000${this.nextLegacySourceId++}`
            : this.sourceKey(responseKey, sourceIndex, index);
          active = {
            key,
            responseKey,
            outputIndex: sourceIndex,
            partIndex: index,
            content: "",
            codeUnits: 0,
            memoryBytes: 0,
          };
          this.activeItems.set(key, active);
        }

        const appendMatchesItem = append?.index === index &&
          (append.outputIndex === undefined || sourceIndex === undefined || append.outputIndex === sourceIndex);
        if (appendMatchesItem) {
          capacity = this.appendContribution(state, active, append.append);
          if (append.append.length > 0) changedContent.add(index);
        } else if (isNew) {
          const prefixLength = this.baseLength(state) + this.contributionLength(state);
          const initial = content.value.slice(Math.min(prefixLength, content.value.length));
          capacity = this.appendContribution(state, active, initial);
          if (initial.length > 0) changedContent.add(index);
        } else {
          const otherContentLength = this.baseLength(state) + state.contributions
            .filter((contribution) => contribution.itemKey !== active!.key)
            .reduce((total, contribution) => total + contribution.codeUnits, 0);
          const currentItemLength = state.contributions
            .filter((contribution) => contribution.itemKey === active!.key)
            .reduce((total, contribution) => total + contribution.codeUnits, 0);
          const nextItemLength = content.value.length - otherContentLength;
          const partIsOnlyThisItem = state.contributions.every(
            (contribution) => contribution.itemKey === active!.key,
          );
          if (partIsOnlyThisItem && nextItemLength >= 0 && nextItemLength !== currentItemLength) {
            const replacement = content.value.slice(this.baseLength(state));
            capacity = this.replaceContribution(state, active, replacement);
            if (replacement.length > 0) changedContent.add(index);
          }
          // A non-appending full snapshot for one item in a merged/interleaved
          // part is ambiguous. Keep the last streamed value until its staged
          // reader becomes authoritative instead of mixing item contents.
        }
      }

      this.latestShape = stripContent(message);
      if (!this.spilled) {
        this.memoryMessage = message;
      } else {
        this.diskMessage = this.latestShape;
        this.memoryMessage = undefined;
      }
      if (this.activeMemoryBytes > this.options.memoryBytes) {
        capacity = Promise.all([capacity, this.spillActiveContent()]).then(() => undefined);
      }
      return {
        metadata: this.memoryMessage ?? this.diskMessage ?? this.latestShape,
        changedContent,
        capacity: this.observeCapacity(capacity, message),
      };
    } catch (error) {
      this.disableAfterError(error, message);
      return {
        metadata: message,
        changedContent: new Set(message.parts.map((_part, partIndex) => partIndex)),
        capacity: Promise.resolve(),
      };
    }
  }

  public setFallbackMessageProvider(provider: () => WebUiMessage | undefined): void {
    this.fallbackMessageProvider = provider;
  }

  /** Replace only top-level metadata such as the pending interaction. */
  public setMetadata(message: WebUiMessage): void {
    this.latestShape = stripContent(message);
    if (this.spilled) {
      this.diskMessage = this.latestShape;
    } else {
      this.memoryMessage = message;
      this.baselineMessage = message;
    }
  }

  public read(): Promise<WebUiMessage | undefined> {
    const read = this.readSnapshot().catch((error: unknown) => {
      const fallback = this.fallbackMessageProvider?.() ?? this.memoryMessage ?? this.baselineMessage;
      if (fallback === undefined) throw error;
      this.disableAfterError(error, fallback);
      return fallback;
    });
    this.activeReads.add(read);
    return read.finally(() => this.activeReads.delete(read));
  }

  /** Rebuilds content from stable staged/partial sources, never from blank metadata. */
  private async readSnapshot(): Promise<WebUiMessage | undefined> {
    if (this.disabled) return this.fallbackMessageProvider?.() ?? this.memoryMessage;
    if (this.memoryMessage !== undefined) return this.memoryMessage;
    const shape = this.diskMessage ?? this.latestShape;
    if (shape === undefined) return this.baselineMessage;

    const manifest = cloneMessage(shape);
    const baseline = this.baselineMessage;
    const contexts = new Map<number, PartContentState>();
    const stagedByKey = new Map<string, StagedContentSource>();
    for (const [index, state] of this.partContents) {
      const copied: PartContentState = {
        ...state,
        contributions: state.contributions.map((contribution) => ({...contribution})),
        stagedSources: new Map(state.stagedSources),
      };
      contexts.set(index, copied);
      for (const [key, source] of copied.stagedSources) stagedByKey.set(key, source);
    }
    const activeByKey = new Map(
      [...this.activeItems].map(([key, source]) => [key, {...source}] as const),
    );
    const partialContent = this.partialWriter === undefined
      ? new Map<string, string>()
      : await this.partialWriter.readItems();
    const stagedReads = new Map<string, Promise<string>>();
    const readContent = (key: string): Promise<string> => {
      const staged = stagedByKey.get(key);
      if (staged !== undefined) {
        let existing = stagedReads.get(key);
        if (existing === undefined) {
          existing = staged.read();
          stagedReads.set(key, existing);
        }
        return existing;
      }
      return Promise.resolve(partialContent.get(key) ?? activeByKey.get(key)?.content ?? "");
    };

    const loaded = await Promise.all(manifest.parts.map(async (part, index) => {
      const state = contexts.get(index);
      const basePart = baseline?.parts[index];
      const baseValue = state === undefined
        ? partContent(basePart)?.value ?? ""
        : state.includeBase ? state.baseValue : "";
      if (state === undefined) return [index, baseValue] as const;
      const pieces = await Promise.all(state.contributions.map(async (contribution) => {
        const content = await readContent(contribution.itemKey);
        return content.slice(contribution.offset, contribution.offset + contribution.codeUnits);
      }));
      return [index, baseValue + pieces.join("")] as const;
    }));
    const contentByPart = new Map(loaded);
    const restoredParts = manifest.parts.map((part, index): WebUiPart => {
      const value = contentByPart.get(index) ?? "";
      const state = contexts.get(index);
      const field = state?.field ?? partContent(part)?.field;
      if (field === "text" && (part.type === "text" || part.type === "reasoning")) {
        return {...part, text: value};
      }
      if (field === "arguments" && part.type === "tool_call") {
        return {...part, arguments: value};
      }
      if (field === "output" && part.type === "tool_result") {
        return {...part, output: value};
      }
      return part;
    });
    return {...manifest, parts: restoredParts, ...fieldsFromParts(restoredParts)};
  }

  /** Publish the terminal source before asynchronously retiring partial data. */
  public async finish(message?: WebUiMessage): Promise<void> {
    const reads = [...this.activeReads];
    this.currentResponseKey = undefined;
    this.partialDirectory = undefined;
    this.partContents.clear();
    this.clearActiveItems();
    this.spilled = false;
    this.diskMessage = undefined;
    if (message !== undefined) {
      this.baselineMessage = message;
      this.memoryMessage = message;
      this.latestShape = message;
    } else if (this.memoryMessage === undefined) {
      this.baselineMessage = undefined;
      this.latestShape = undefined;
    }
    await Promise.allSettled(reads);
    await this.cleanupPartialFile();
  }

  /** Remove temporary cache data when a run is no longer attachable. */
  public async dispose(): Promise<void> {
    this.disabled = true;
    const reads = [...this.activeReads];
    this.partContents.clear();
    this.clearActiveItems();
    this.memoryMessage = undefined;
    this.diskMessage = undefined;
    this.baselineMessage = undefined;
    this.latestShape = undefined;
    this.currentResponseKey = undefined;
    this.partialDirectory = undefined;
    this.spilled = false;
    await Promise.allSettled(reads);
    await this.cleanupPartialFile();
  }

  private createPartState(index: number, part: WebUiPart): PartContentState {
    const baselineValue = this.baselinePartContent(index, part);
    return {
      field: partContent(part)!.field,
      type: part.type,
      baseValue: baselineValue ?? "",
      includeBase: baselineValue !== undefined,
      contributions: [],
      stagedSources: new Map(),
    };
  }

  private baselinePartContent(index: number, part: WebUiPart): string | undefined {
    const baselinePart = this.baselineMessage?.parts[index];
    if (baselinePart === undefined || baselinePart.type !== part.type) return undefined;
    if (
      (part.type === "tool_call" && baselinePart.type === "tool_call" && part.callId !== baselinePart.callId) ||
      (part.type === "tool_result" && baselinePart.type === "tool_result" && part.callId !== baselinePart.callId)
    ) {
      return undefined;
    }
    const value = partContent(baselinePart);
    const current = partContent(part);
    return value !== undefined && current !== undefined && value.field === current.field
      ? value.value
      : undefined;
  }

  private findActiveSource(
    responseKey: string,
    outputIndex: number | undefined,
    partIndex: number,
  ): ActiveContentSource | undefined {
    if (outputIndex !== undefined) {
      return this.activeItems.get(this.sourceKey(responseKey, outputIndex, partIndex));
    }
    const matches = [...this.activeItems.values()].filter(
      (item) => item.responseKey === responseKey && item.outputIndex === undefined && item.partIndex === partIndex,
    );
    return matches.length === 1 ? matches[0] : undefined;
  }

  private sourceKey(responseKey: string, outputIndex: number | undefined, partIndex: number): string {
    return `${responseKey}\u0000${outputIndex === undefined ? "legacy" : outputIndex}\u0000${partIndex}`;
  }

  private appendContribution(
    state: PartContentState,
    source: ActiveContentSource,
    value: string,
  ): Promise<void> {
    if (value.length === 0) return Promise.resolve();
    const offset = source.codeUnits;
    const last = state.contributions.at(-1);
    if (last?.itemKey === source.key && last.offset + last.codeUnits === offset) {
      last.codeUnits += value.length;
    } else {
      if (this.contributionCount() >= this.maxIndexSegments) {
        throw new Error("response partial item-range index limit exceeded");
      }
      state.contributions.push({itemKey: source.key, offset, codeUnits: value.length});
    }
    source.codeUnits += value.length;
    return this.appendActiveContent(source, value);
  }

  private replaceContribution(
    state: PartContentState,
    source: ActiveContentSource,
    value: string,
  ): Promise<void> {
    const positions = state.contributions
      .map((contribution, index) => contribution.itemKey === source.key ? index : -1)
      .filter((index) => index >= 0);
    const insertion = positions[0] ?? state.contributions.length;
    state.contributions = state.contributions.filter((contribution) => contribution.itemKey !== source.key);
    if (value.length > 0) {
      if (positions.length === 0 && this.contributionCount() >= this.maxIndexSegments) {
        throw new Error("response partial item-range index limit exceeded");
      }
      state.contributions.splice(insertion, 0, {itemKey: source.key, offset: 0, codeUnits: value.length});
    }
    return this.replaceActiveContent(source, value);
  }

  private reconcileContributionLength(state: PartContentState, key: string, codeUnits: number): void {
    let total = state.contributions
      .filter((contribution) => contribution.itemKey === key)
      .reduce((sum, contribution) => sum + contribution.codeUnits, 0);
    if (total < codeUnits) {
      const last = [...state.contributions].reverse().find((contribution) => contribution.itemKey === key);
      if (last === undefined) {
        if (this.contributionCount() >= this.maxIndexSegments) {
          throw new Error("response partial item-range index limit exceeded");
        }
        state.contributions.push({itemKey: key, offset: 0, codeUnits});
      } else {
        last.codeUnits += codeUnits - total;
      }
      return;
    }
    let remove = total - codeUnits;
    for (let index = state.contributions.length - 1; index >= 0 && remove > 0; index -= 1) {
      const contribution = state.contributions[index]!;
      if (contribution.itemKey !== key) continue;
      const amount = Math.min(contribution.codeUnits, remove);
      contribution.codeUnits -= amount;
      remove -= amount;
      if (contribution.codeUnits === 0) state.contributions.splice(index, 1);
    }
  }

  private contributionCount(): number {
    let count = 0;
    for (const state of this.partContents.values()) count += state.contributions.length;
    return count;
  }

  private baseLength(state: PartContentState): number {
    return state.includeBase ? state.baseValue.length : 0;
  }

  private contributionLength(state: PartContentState): number {
    return state.contributions.reduce((total, contribution) => total + contribution.codeUnits, 0);
  }

  private appendActiveContent(source: ActiveContentSource, delta: string): Promise<void> {
    if (this.partialWriter !== undefined) return this.partialWriter.append(source.key, delta);
    source.content += delta;
    source.memoryBytes += Buffer.byteLength(delta, "utf8");
    this.activeMemoryBytes += Buffer.byteLength(delta, "utf8");
    return Promise.resolve();
  }

  private replaceActiveContent(source: ActiveContentSource, value: string): Promise<void> {
    this.activeMemoryBytes = Math.max(0, this.activeMemoryBytes - source.memoryBytes);
    source.content = "";
    source.memoryBytes = 0;
    source.codeUnits = value.length;
    if (this.partialWriter !== undefined) return this.partialWriter.replace(source.key, value);
    source.content = value;
    source.memoryBytes = Buffer.byteLength(value, "utf8");
    this.activeMemoryBytes += source.memoryBytes;
    return Promise.resolve();
  }

  private spillActiveContent(): Promise<void> {
    if (this.disabled || this.partialWriter !== undefined || this.activeMemoryBytes <= this.options.memoryBytes) {
      return Promise.resolve();
    }
    const directory = this.partialDirectory ?? this.options.directory;
    if (directory === undefined) return Promise.resolve();

    this.partialFilePath = join(directory, RESPONSE_PARTIAL_FILE);
    const writer = new ResponsePartialWriter({
      path: this.partialFilePath,
      batchBytes: this.writeBatchBytes,
      maxBufferedBytes: this.options.memoryBytes === 0
        ? this.writeBatchBytes
        : this.options.memoryBytes,
      flushIntervalMs: this.flushIntervalMs,
      maxIndexSegments: this.maxIndexSegments,
      onError: (error) => this.handleWriterError(error),
    });
    this.partialWriter = writer;
    this.spilled = true;
    this.memoryMessage = undefined;
    this.diskMessage = this.latestShape;

    const initial: Array<{itemKey: string; takeContent: () => string}> = [];
    for (const source of this.activeItems.values()) {
      if (source.content.length === 0) continue;
      initial.push({
        itemKey: source.key,
        takeContent: () => {
          const content = source.content;
          source.content = "";
          this.activeMemoryBytes = Math.max(0, this.activeMemoryBytes - source.memoryBytes);
          source.memoryBytes = 0;
          return content;
        },
      });
    }
    return writer.appendItems(initial);
  }

  private async cleanupPartialFile(): Promise<void> {
    await this.clearPartialFile();
  }

  private clearPartialFile(): Promise<void> {
    const writer = this.partialWriter;
    if (writer === undefined) return this.partialCleanupPromise ?? Promise.resolve();
    this.partialWriter = undefined;
    this.partialFilePath = undefined;
    this.partialCleanupPromise = writer.dispose().catch((error: unknown) => {
      console.warn(`[web-ui-run-cache] failed to dispose partial cache: ${error instanceof Error ? error.message : String(error)}`);
    });
    return this.partialCleanupPromise;
  }

  private observeCapacity(capacity: Promise<void>, message: WebUiMessage): Promise<void> {
    return capacity.catch((error: unknown) => {
      this.disableAfterError(error, this.fallbackMessageProvider?.() ?? message);
    });
  }

  private handleWriterError(error: unknown): void {
    const fallback = this.fallbackMessageProvider?.() ?? this.memoryMessage ?? this.baselineMessage ?? this.latestShape;
    if (fallback !== undefined) this.disableAfterError(error, fallback);
    else console.warn(`[web-ui-run-cache] spill disabled: ${error instanceof Error ? error.message : String(error)}`);
  }

  private clearActiveItems(): void {
    this.activeItems.clear();
    this.activeMemoryBytes = 0;
  }

  private disableAfterError(error: unknown, newest: WebUiMessage): void {
    if (this.disabled) return;
    console.warn(`[web-ui-run-cache] spill disabled: ${error instanceof Error ? error.message : String(error)}`);
    const writer = this.partialWriter;
    this.partialWriter = undefined;
    this.partialFilePath = undefined;
    if (writer !== undefined) {
      this.partialCleanupPromise = writer.dispose().catch((cleanupError: unknown) => {
        console.warn(`[web-ui-run-cache] partial cleanup failed: ${cleanupError instanceof Error ? cleanupError.message : String(cleanupError)}`);
      });
    }
    this.partContents.clear();
    this.clearActiveItems();
    this.diskMessage = undefined;
    this.memoryMessage = newest;
    this.baselineMessage = newest;
    this.latestShape = newest;
    this.spilled = false;
    this.disabled = true;
  }
}

function partContent(part: WebUiPart | undefined): {field: ContentField; value: string} | undefined {
  if (part === undefined) return undefined;
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
    ...(message.pending === undefined ? {} : {pending: {...message.pending}}),
  };
}
