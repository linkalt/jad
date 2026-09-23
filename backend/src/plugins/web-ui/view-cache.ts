import { stat } from "node:fs/promises";

import type { JsonRecord, SessionResponseStore } from "../../persistence.js";
import {
  createWebUiSessionState,
  isUserTurnRecord,
  reduceWebUiRecord,
} from "./normalize-web-ui.js";
import { inputItemRecord } from "./shared-web-ui.js";
import type { WebUiMessage } from "./types.js";

/** A slice of a session transcript projected for the web UI. */
export interface WebUiSessionView {
  messages: WebUiMessage[];
  /** Older turns exist in the transcript but are not part of this response. */
  hasMore: boolean;
  /** Cursor for the next older page; `null` when the transcript is exhausted. */
  nextBefore: number | null;
}

export interface WebUiViewOptions {
  /** Turns returned when no cursor is given; `0` returns the whole transcript. */
  snapshotTurns: number;
  /** Upper bound for one page, so a cursor cannot ask for everything at once. */
  maxPageTurns: number;
}

interface HeadCache {
  key: string;
  messages: WebUiMessage[];
}

function parseRecord(line: string): JsonRecord | undefined {
  try {
    const value: unknown = JSON.parse(line);
    return typeof value === "object" && value !== null && !Array.isArray(value)
      ? (value as JsonRecord)
      : undefined;
  } catch {
    return undefined;
  }
}

/**
 * Projection of a session transcript for the web UI, addressed by byte offset.
 *
 * Two parts are read separately. The compacted prefix lives in the compaction
 * cache as request items and is folded once per compaction; the live tail is read
 * from the transcript in turn-sized slices, using the cursor index to find where
 * a turn starts without walking the file from the beginning.
 *
 * Because every slice begins at a turn boundary and records are identified by
 * their offset, a page contains exactly the messages a full read would produce
 * for the same turns — loading older pages extends the transcript instead of
 * renumbering it.
 */
export class WebUiSessionViewCache {
  private head: HeadCache | undefined;

  public constructor(
    private readonly store: SessionResponseStore,
    private readonly compactionCachePath: string,
    private readonly options: WebUiViewOptions,
  ) {}

  public async read(options: {before?: number; turns?: number} = {}): Promise<WebUiSessionView> {
    const stamp = await this.store.readTranscriptStamp();
    const head = await this.readHead();
    const page = await this.readPage(stamp.size, options);
    // A cursor asks for older tail turns only: the caller already holds the head.
    if (options.before !== undefined) {
      return page;
    }
    return {...page, messages: [...head.messages, ...page.messages]};
  }

  /**
   * Reads the turns that end at `end`, newest last.
   *
   * `before` selects the turns older than a previous page's start offset, and
   * `turns` caps how many are returned; together they tile the tail exactly, so
   * every page is reproducible from the cursor alone.
   */
  private async readPage(
    end: number,
    options: {before?: number; turns?: number},
  ): Promise<WebUiSessionView> {
    const requested = options.turns ?? this.options.snapshotTurns;
    const limit = requested <= 0 ? 0 : Math.min(requested, this.options.maxPageTurns);
    const starts = await this.turnStarts(end);
    const before = options.before;
    const last = before === undefined
      ? starts.length
      : starts.findIndex((start) => start >= before);
    const endIndex = last < 0 ? starts.length : last;

    if (endIndex <= 0) {
      return {messages: [], hasMore: false, nextBefore: null};
    }
    const first = limit <= 0 || limit >= endIndex ? 0 : endIndex - limit;
    const start = starts[first]!;
    const stop = endIndex >= starts.length ? end : starts[endIndex]!;

    const state = createWebUiSessionState();
    await this.store.readRecords(start, stop, (offset, line) => {
      const record = parseRecord(line);
      if (record !== undefined) {
        reduceWebUiRecord(state, record, `o${offset}`);
      }
    });

    const hasMore = first > 0;
    return {messages: state.messages, hasMore, nextBefore: hasMore ? start : null};
  }

  /**
   * Offsets where each turn's records begin, oldest first. A turn opens with its
   * user message when the transcript has one, and with the provider run itself
   * otherwise (a transcript can start mid-turn after compaction).
   */
  private async turnStarts(end: number): Promise<number[]> {
    const index = await this.store.readCursorIndex();
    const starts: number[] = [0];
    for (const run of index.runs) {
      if (run.step !== 1 || run.offset <= 0 || run.offset >= end) {
        continue;
      }
      const previous = await this.store.readRecordStartBefore(run.offset);
      const start = previous === undefined
        ? run.offset
        : await this.readTurnStart(previous, run.offset);
      if (start > starts[starts.length - 1]!) {
        starts.push(start);
      }
    }
    return starts;
  }

  /** The turn's own offset: the user message in front of the run, when present. */
  private async readTurnStart(offset: number, runOffset: number): Promise<number> {
    let record: JsonRecord | undefined;
    await this.store.readRecords(offset, runOffset, (_offset, line) => {
      record ??= parseRecord(line);
    });
    return record !== undefined && isUserTurnRecord(record) ? offset : runOffset;
  }

  private async readHead(): Promise<HeadCache> {
    const key = await this.compactionStamp();
    if (this.head !== undefined && this.head.key === key) {
      return this.head;
    }
    const items = (await this.store.readCompactionItems()) ?? [];
    const state = createWebUiSessionState();
    items.forEach((item, index) => {
      const record = inputItemRecord(item);
      if (record !== undefined) {
        reduceWebUiRecord(state, record, `h${index}`);
      }
    });
    this.head = {key, messages: state.messages};
    return this.head;
  }

  private async compactionStamp(): Promise<string> {
    try {
      const info = await stat(this.compactionCachePath);
      return [info.dev, info.ino, info.size, Math.trunc(info.mtimeMs)].join(":");
    } catch {
      return "none";
    }
  }
}
