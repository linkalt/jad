/**
 * Remembering how tall each transcript row really is.
 *
 * A deferred row (`content-visibility: auto`) renders at an estimate until the
 * engine lays it out for the first time. When that happens the scroll range
 * changes by the difference between the estimate and the truth, and the reader
 * — who is usually mid-scroll — sees the transcript move: measured on a real
 * session, one row's estimate being replaced shifted the range by 695–927px,
 * and scrolling past a handful of rows produced one such shift per 200–1500ms.
 * `contain-intrinsic-size: auto` only remembers a size for as long as the DOM
 * element lives, and any re-render creates new elements, so the range changes
 * again the next time those rows are laid out.
 *
 * This store keeps the measured height of every row it has seen and hands it
 * back, so a row starts at the height it had last time instead of at a guess.
 * Values are bucketed by the width they were measured at, because wrapping
 * changes a row's height and a stored height from a different width would be a
 * fresh error of its own.
 */

/** The slice of `localStorage` this store needs, so a test can stand in. */
export interface RowHeightStorage {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
}

/** One remembered row: id, measured height, and the width it was measured at. */
export type RowHeightEntry = [id: string, height: number, width: number];

export interface RowHeightStoreOptions {
  storage: RowHeightStorage;
  storageKey: string;
  /** Entries kept; the oldest are dropped past this. */
  maxEntries: number;
  /** Writes are batched: a scroll measures many rows in a row. */
  saveDelayMs: number;
  /** Widths are rounded to this, so small deltas still match. */
  widthBucketPx: number;
  schedule?: (callback: () => void, delayMs: number) => void;
  now?: () => number;
}

export interface RowHeightStore {
  /** The remembered height, or null when this row is unknown at this width. */
  heightFor(id: string, width: number): number | null;
  remember(id: string, height: number, width: number): void;
  /** Writes a pending change immediately. */
  flush(): void;
  size(): number;
}

/** A row taller than this is a measurement gone wrong, not a transcript row. */
const MAX_ROW_HEIGHT_PX = 100000;

/** Shape of what is stored, so a future change can migrate instead of guess. */
const STORAGE_VERSION = 1;

export function bucketWidth(width: number, bucketPx: number): number {
  if (!Number.isFinite(width) || width <= 0 || bucketPx <= 0) {
    return 0;
  }
  return Math.round(width / bucketPx) * bucketPx;
}

export function usableHeight(height: number): boolean {
  return Number.isFinite(height) && height > 0 && height <= MAX_ROW_HEIGHT_PX;
}

/** Reads stored rows, ignoring anything that is not a usable measurement. */
export function parseRowHeights(raw: string | null): RowHeightEntry[] {
  if (raw === null) {
    return [];
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return [];
  }
  if (typeof parsed !== 'object' || parsed === null) {
    return [];
  }
  const {v, rows} = parsed as {v?: unknown; rows?: unknown};
  if (v !== STORAGE_VERSION || !Array.isArray(rows)) {
    return [];
  }
  const entries: RowHeightEntry[] = [];
  for (const row of rows) {
    if (!Array.isArray(row) || row.length !== 3) {
      continue;
    }
    const [id, height, width] = row as [unknown, unknown, unknown];
    if (typeof id !== 'string' || id.length === 0 || typeof height !== 'number') {
      continue;
    }
    if (!usableHeight(height)) {
      continue;
    }
    entries.push([id, height, typeof width === 'number' && width >= 0 ? width : 0]);
  }
  return entries;
}

export function createRowHeightStore(options: RowHeightStoreOptions): RowHeightStore {
  const {storage, storageKey, maxEntries, saveDelayMs, widthBucketPx} = options;
  const schedule = options.schedule ?? ((callback: () => void, delayMs: number) => window.setTimeout(callback, delayMs));

  let loaded = false;
  const heights = new Map<string, {height: number; width: number}>();
  let saveScheduled = false;

  const load = () => {
    if (loaded) {
      return;
    }
    loaded = true;
    for (const [id, height, width] of parseRowHeights(storage.getItem(storageKey))) {
      heights.set(id, {height, width});
    }
    trim();
  };

  const trim = () => {
    while (heights.size > maxEntries) {
      const oldest = heights.keys().next();
      if (oldest.done === true) {
        break;
      }
      heights.delete(oldest.value);
    }
  };

  const serialize = (): string => {
    const rows: RowHeightEntry[] = [];
    for (const [id, entry] of heights) {
      rows.push([id, Math.round(entry.height * 10) / 10, entry.width]);
    }
    return JSON.stringify({v: STORAGE_VERSION, rows});
  };

  const write = () => {
    saveScheduled = false;
    trim();
    try {
      storage.setItem(storageKey, serialize());
    } catch {
      // A full or blocked store is not worth failing a render over: the rows
      // simply go back to their estimate.
    }
  };

  const scheduleSave = () => {
    if (saveScheduled) {
      return;
    }
    saveScheduled = true;
    schedule(write, saveDelayMs);
  };

  return {
    heightFor(id, width) {
      load();
      const entry = heights.get(id);
      if (entry === undefined) {
        return null;
      }
      // Width 0 means the caller has not measured a row yet this session; the
      // height is still a better starting point than the configured estimate.
      const wanted = bucketWidth(width, widthBucketPx);
      if (wanted !== 0 && entry.width !== 0 && Math.abs(wanted - entry.width) > widthBucketPx) {
        return null;
      }
      return entry.height;
    },
    remember(id, height, width) {
      load();
      if (id.length === 0 || !usableHeight(height)) {
        return;
      }
      const bucketed = bucketWidth(width, widthBucketPx);
      const previous = heights.get(id);
      const rounded = Math.round(height * 10) / 10;
      if (previous !== undefined && previous.height === rounded && previous.width === bucketed) {
        return;
      }
      if (previous !== undefined) {
        // Re-insert so the trim order reflects what was measured last.
        heights.delete(id);
      }
      heights.set(id, {height: rounded, width: bucketed});
      scheduleSave();
    },
    flush() {
      write();
    },
    size() {
      load();
      return heights.size;
    },
  };
}
