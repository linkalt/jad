export interface StorageKeyMigration {
  from: string;
  to: string;
  area: 'local' | 'session';
}

export interface KeyValueStorage {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
  removeItem(key: string): void;
}

/** Copy each old key's value to its new key once, then delete the old key.
 * A value already stored under the new key wins; the old one is just removed. */
export function migrateStorageKeys(
  migrations: readonly StorageKeyMigration[],
  areas: {local?: KeyValueStorage | null; session?: KeyValueStorage | null},
): number {
  let moved = 0;
  for (const {from, to, area} of migrations) {
    const storage = areas[area];
    if (!storage) continue;
    try {
      const value = storage.getItem(from);
      if (value === null) continue;
      if (storage.getItem(to) === null) {
        storage.setItem(to, value);
        moved += 1;
      }
      storage.removeItem(from);
    } catch {
      // Storage may be blocked or full; the app then starts with defaults.
    }
  }
  return moved;
}
