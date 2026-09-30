import {
  closeSync,
  mkdirSync,
  openSync,
  readFileSync,
  rmSync,
  writeSync,
} from "node:fs";
import {join} from "node:path";

import type {WebUiMessage, WebUiPart} from "./plugins/web-ui/types.js";
import {fieldsFromParts} from "./plugins/web-ui/shared-web-ui.js";
import type {WebUiMessageAppend} from "./plugins/web-ui/incremental-web-ui.js";

export interface RunContentCacheUpdate {
  metadata: WebUiMessage;
  changedContent: ReadonlySet<number>;
}

export interface RunContentCacheStagedReader {
  read: () => Promise<string>;
  codeUnits: number;
}

interface RunContentCacheOptions {
  /** Profile-scoped fallback staging root when no response-turn path is supplied. */
  directory?: string;
  /** UTF-8 bytes of the active output item kept in memory before spilling. */
  memoryBytes: number;
}

type ContentField = "text" | "arguments" | "output";

interface StagedContentSource {
  responseKey: string;
  outputIndex: number;
  codeUnits: number;
  read: () => Promise<string>;
}

interface PartContentState {
  field: ContentField;
  type: WebUiPart["type"];
  baseValue: string;
  includeBase: boolean;
  stagedSources: StagedContentSource[];
  stagedCodeUnits: number;
}

/**
 * Keeps a normalized message scheme in memory and one active output item's
 * content delta. When the delta exceeds its budget, only UTF-16LE content is
 * appended to `responses.partial`; there are no event envelopes or per-chunk
 * records. A durably staged item becomes the replay source and retires the file.
 */
export class RunContentCache {
  private memoryMessage: WebUiMessage | undefined;
  private diskMessage: WebUiMessage | undefined;
  private baselineMessage: WebUiMessage | undefined;
  private latestShape: WebUiMessage | undefined;
  private readonly partContents = new Map<number, PartContentState>();
  private currentResponseKey: string | undefined;
  private partialDirectory: string | undefined;
  private partialFilePath: string | undefined;
  private partialFileDescriptor: number | undefined;
  private activePartIndex: number | undefined;
  private activeOutputIndex: number | undefined;
  private activeResponseKey: string | undefined;
  private activeContent = "";
  private activeMemoryBytes = 0;
  private spilled = false;
  private disabled = false;

  public constructor(private readonly options: RunContentCacheOptions) {}

  public get isSpilled(): boolean {
    return this.spilled;
  }

  /** Latest message or its content-free normalized scheme. */
  public metadata(): WebUiMessage | undefined {
    return this.memoryMessage ?? this.diskMessage ?? this.baselineMessage;
  }

  /** Starts the partial-content lifetime for one Responses turn. */
  public beginResponse(
    directory: string,
    responseKey: string,
    baselineMessage?: WebUiMessage,
  ): void {
    const candidate = baselineMessage ?? this.memoryMessage ?? this.baselineMessage;
    const baseline = candidate !== undefined && candidate.parts.length === 0 && candidate.text === ""
      ? undefined
      : candidate;
    this.cleanupPartialFile();
    this.currentResponseKey = responseKey;
    this.partialDirectory = directory;
    this.baselineMessage = baseline;
    this.memoryMessage = baseline;
    this.latestShape = baseline;
    this.diskMessage = undefined;
    this.partContents.clear();
    this.clearActiveItem();
    this.spilled = false;
  }

  /**
   * Replaces the active item's streamed delta with the authoritative staged
   * output reader, then deletes its partial content file.
   */
  public handoffOutputItem(
    responseKey: string,
    outputIndex: number,
    readers: ReadonlyMap<number, RunContentCacheStagedReader>,
  ): void {
    if (responseKey !== this.currentResponseKey) return;

    const visibleMessage = this.memoryMessage ?? this.latestShape ?? this.baselineMessage;
    for (const [partIndex, reader] of readers) {
      const part = visibleMessage?.parts[partIndex] ?? this.baselineMessage?.parts[partIndex];
      if (part === undefined) continue;
      const state = this.partContents.get(partIndex) ?? this.createPartState(partIndex, part);
      const duplicate = state.stagedSources.some(
        (source) => source.responseKey === responseKey && source.outputIndex === outputIndex,
      );
      if (!duplicate) {
        state.stagedSources.push({
          responseKey,
          outputIndex,
          codeUnits: reader.codeUnits,
          read: reader.read,
        });
        state.stagedCodeUnits += reader.codeUnits;
      }
      this.partContents.set(partIndex, state);
    }

    const itemMatches = this.activeResponseKey === responseKey &&
      (this.activeOutputIndex === undefined || this.activeOutputIndex === outputIndex);
    if (itemMatches) {
      this.clearPartialFile();
      this.clearActiveItem();
    }

    if (this.memoryMessage !== undefined && readers.size > 0) {
      this.latestShape = stripContent(this.memoryMessage);
      this.diskMessage = this.latestShape;
      this.memoryMessage = undefined;
    }
    this.spilled = this.spilled || readers.size > 0;
  }

  /** The staged output has been committed to messages.jsonl. */
  public commitResponse(responseKey: string, message: WebUiMessage): void {
    if (responseKey !== this.currentResponseKey) return;
    this.cleanupPartialFile();
    this.currentResponseKey = undefined;
    this.partialDirectory = undefined;
    this.partContents.clear();
    this.clearActiveItem();
    this.baselineMessage = message;
    this.memoryMessage = message;
    this.diskMessage = undefined;
    this.latestShape = message;
    this.spilled = false;
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
      return {metadata: message, changedContent: new Set()};
    }

    const responseKey = this.currentResponseKey;
    const sourceIndex = append?.outputIndex ?? outputIndex;
    const activePartIndex = sourceIndex !== undefined &&
      this.activeResponseKey === responseKey &&
      (this.activeOutputIndex === undefined || this.activeOutputIndex === sourceIndex)
      ? this.activePartIndex
      : undefined;
    const index = append?.index ?? partIndex ?? activePartIndex;
    const changedContent = new Set<number>();
    const part = index === undefined ? undefined : message.parts[index];
    const content = partContent(part);
    if (index !== undefined && part !== undefined && content !== undefined) {
      const state = this.partContents.get(index) ?? this.createPartState(index, part);
      this.partContents.set(index, state);
      const sameActiveItem = this.activePartIndex === index &&
        this.activeResponseKey === responseKey &&
        (this.activeOutputIndex === undefined || sourceIndex === undefined || this.activeOutputIndex === sourceIndex);

      if (!sameActiveItem) {
        // Responses streams stage one output item before starting the next. If a
        // provider violates that sequence, retain the newest complete snapshot
        // in memory rather than mixing two items in the single partial file.
        if (this.activePartIndex !== undefined &&
          (this.activeContent.length > 0 || this.partialFilePath !== undefined)) {
          this.disableAfterError(new Error("a new output item started before the previous item was staged"), message);
          return {
            metadata: message,
            changedContent: new Set(message.parts.map((_part, partIndex) => partIndex)),
          };
        }
        this.activePartIndex = index;
        this.activeResponseKey = responseKey;
        this.activeOutputIndex = sourceIndex;
        const prefixLength = (state.includeBase ? state.baseValue.length : 0) + state.stagedCodeUnits;
        const initial = append?.index === index
          ? append.append
          : content.value.slice(Math.min(prefixLength, content.value.length));
        this.replaceActiveContent(initial);
        if (initial.length > 0) changedContent.add(index);
      } else if (append?.index === index) {
        if (sourceIndex !== undefined) this.activeOutputIndex = sourceIndex;
        this.appendActiveContent(append.append);
        if (append.append.length > 0) changedContent.add(index);
      } else {
        const prefixLength = (state.includeBase ? state.baseValue.length : 0) + state.stagedCodeUnits;
        const replacement = content.value.slice(Math.min(prefixLength, content.value.length));
        this.replaceActiveContent(replacement);
        if (replacement.length > 0) changedContent.add(index);
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
      this.spillActiveContent();
    }
    return {
      metadata: this.memoryMessage ?? this.diskMessage ?? this.latestShape,
      changedContent,
    };
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

  /** Materialize the latest snapshot from its baseline, staged items and active delta. */
  public async read(): Promise<WebUiMessage | undefined> {
    if (this.memoryMessage !== undefined) return this.memoryMessage;
    const shape = this.diskMessage ?? this.latestShape;
    if (shape === undefined) return this.baselineMessage;

    const manifest = cloneMessage(shape);
    const baseline = this.baselineMessage;
    const contexts = new Map<number, PartContentState>();
    for (const [index, state] of this.partContents) {
      contexts.set(index, {
        ...state,
        stagedSources: state.stagedSources.slice(),
      });
    }
    const activePartIndex = this.activePartIndex;
    const activeMemoryContent = this.activeContent;
    const partialContent = this.partialFilePath === undefined
      ? ""
      : readFileSync(this.partialFilePath).toString("utf16le");

    const loaded = await Promise.all(manifest.parts.map(async (part, index) => {
      const state = contexts.get(index);
      const basePart = baseline?.parts[index];
      const baseValue = state === undefined
        ? partContent(basePart)?.value ?? ""
        : state.includeBase ? state.baseValue : "";
      const staged = state === undefined
        ? []
        : await Promise.all(state.stagedSources.map((source) => source.read()));
      const active = activePartIndex === index ? partialContent + activeMemoryContent : "";
      return [index, baseValue + staged.join("") + active] as const;
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

  /** Keep a final terminal message in memory and retire temporary partial data. */
  public finish(message?: WebUiMessage): void {
    this.cleanupPartialFile();
    this.currentResponseKey = undefined;
    this.partialDirectory = undefined;
    this.partContents.clear();
    this.clearActiveItem();
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
  }

  /** Remove temporary cache data when a run is no longer attachable. */
  public dispose(): void {
    this.disabled = true;
    this.cleanupPartialFile();
    this.partContents.clear();
    this.clearActiveItem();
    this.memoryMessage = undefined;
    this.diskMessage = undefined;
    this.baselineMessage = undefined;
    this.latestShape = undefined;
    this.currentResponseKey = undefined;
    this.partialDirectory = undefined;
    this.spilled = false;
  }

  private createPartState(index: number, part: WebUiPart): PartContentState {
    const baselineValue = this.baselinePartContent(index, part);
    return {
      field: partContent(part)!.field,
      type: part.type,
      baseValue: baselineValue ?? "",
      includeBase: baselineValue !== undefined,
      stagedSources: [],
      stagedCodeUnits: 0,
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

  private appendActiveContent(delta: string): void {
    if (delta.length === 0) return;
    if (this.partialFileDescriptor !== undefined) {
      this.writePartial(delta);
      return;
    }
    this.activeContent += delta;
    this.activeMemoryBytes = Buffer.byteLength(this.activeContent, "utf8");
    if (this.activeMemoryBytes > this.options.memoryBytes) this.spillActiveContent();
  }

  private replaceActiveContent(value: string): void {
    this.clearPartialFile();
    this.activeContent = value;
    this.activeMemoryBytes = Buffer.byteLength(value, "utf8");
    if (this.activeMemoryBytes > this.options.memoryBytes) this.spillActiveContent();
  }

  private spillActiveContent(): void {
    if (this.activeContent.length === 0 || this.disabled) return;
    const directory = this.partialDirectory ?? this.options.directory;
    if (directory === undefined) return;
    mkdirSync(directory, {recursive: true});
    this.partialFilePath ??= join(directory, "responses.partial");
    if (this.partialFileDescriptor === undefined) {
      this.partialFileDescriptor = openSync(this.partialFilePath, "a");
    }
    this.writePartial(this.activeContent);
    this.activeContent = "";
    this.activeMemoryBytes = 0;
    this.spilled = true;
    this.memoryMessage = undefined;
    this.diskMessage = this.latestShape;
  }

  private writePartial(value: string): void {
    if (this.partialFileDescriptor === undefined) {
      const directory = this.partialDirectory ?? this.options.directory;
      if (directory === undefined) throw new Error("response partial directory is unavailable");
      mkdirSync(directory, {recursive: true});
      this.partialFilePath ??= join(directory, "responses.partial");
      this.partialFileDescriptor = openSync(this.partialFilePath, "a");
    }
    const bytes = Buffer.from(value, "utf16le");
    let written = 0;
    while (written < bytes.byteLength) {
      const count = writeSync(
        this.partialFileDescriptor,
        bytes,
        written,
        bytes.byteLength - written,
        null,
      );
      if (count === 0) throw new Error("incomplete response partial append");
      written += count;
    }
  }

  private cleanupPartialFile(): void {
    this.clearPartialFile();
  }

  private clearPartialFile(): void {
    const descriptor = this.partialFileDescriptor;
    this.partialFileDescriptor = undefined;
    if (descriptor !== undefined) {
      try {
        closeSync(descriptor);
      } catch (error) {
        console.warn(`[web-ui-run-cache] failed to close partial cache: ${error instanceof Error ? error.message : String(error)}`);
      }
    }
    const filePath = this.partialFilePath;
    this.partialFilePath = undefined;
    if (filePath !== undefined) rmSync(filePath, {force: true});
  }

  private clearActiveItem(): void {
    this.activePartIndex = undefined;
    this.activeOutputIndex = undefined;
    this.activeResponseKey = undefined;
    this.activeContent = "";
    this.activeMemoryBytes = 0;
  }

  private disableAfterError(error: unknown, newest: WebUiMessage): void {
    console.warn(`[web-ui-run-cache] spill disabled: ${error instanceof Error ? error.message : String(error)}`);
    this.cleanupPartialFile();
    this.partContents.clear();
    this.clearActiveItem();
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
  };
}
