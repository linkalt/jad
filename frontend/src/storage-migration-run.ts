import appConfig from './config.json';
import {migrateStorageKeys, type StorageKeyMigration} from './storage-migration';

function area(read: () => Storage): Storage | null {
  try {
    return read();
  } catch {
    return null;
  }
}

migrateStorageKeys(appConfig.storage_key_migrations as StorageKeyMigration[], {
  local: area(() => window.localStorage),
  session: area(() => window.sessionStorage),
});
