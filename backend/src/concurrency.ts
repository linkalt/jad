// Shared concurrency primitives. AsyncMutex serializes asynchronous operations;
// KeyedMutex does the same per key (for example one file path) and drops idle
// entries so a long running server does not accumulate them.

export class AsyncMutex {
  private tail: Promise<void> = Promise.resolve();

  public async runExclusive<T>(operation: () => Promise<T>): Promise<T> {
    const previous = this.tail;
    let release!: () => void;
    this.tail = new Promise<void>((resolve) => {
      release = resolve;
    });

    await previous;
    try {
      return await operation();
    } finally {
      release();
    }
  }
}

interface KeyedMutexEntry {
  mutex: AsyncMutex;
  users: number;
}

export class KeyedMutex<Key = string> {
  private readonly entries = new Map<Key, KeyedMutexEntry>();

  /** Runs the operation exclusively against other operations using the same key. */
  public async runExclusive<T>(key: Key, operation: () => Promise<T>): Promise<T> {
    const entry = this.retain(key);
    try {
      return await entry.mutex.runExclusive(operation);
    } finally {
      this.release(key, entry);
    }
  }

  private retain(key: Key): KeyedMutexEntry {
    const existing = this.entries.get(key);
    if (existing) {
      existing.users += 1;
      return existing;
    }
    const entry: KeyedMutexEntry = {mutex: new AsyncMutex(), users: 1};
    this.entries.set(key, entry);
    return entry;
  }

  private release(key: Key, entry: KeyedMutexEntry): void {
    entry.users -= 1;
    if (entry.users === 0 && this.entries.get(key) === entry) {
      this.entries.delete(key);
    }
  }
}
