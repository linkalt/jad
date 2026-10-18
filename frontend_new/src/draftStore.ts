import appConfig from './config.json';

/**
 * Persistence for composer drafts.
 *
 * A draft is user content that can be expensive to reproduce — a typed
 * message, a prompt loaded from the library, or an attached file's text — so
 * it lives in `localStorage` under a configured key rather than in memory
 * alone, where a reload would discard it.
 *
 * Drafts are keyed by the session-view key: the main row keeps the legacy
 * session-id key, while each pinned profile row has its own independent draft.
 * Restoring them all keeps switching views and reloading from mixing drafts.
 */
const STORAGE_KEY = appConfig.composer_draft_storage_key;

/** Stored drafts, or an empty set when storage is missing or unreadable. */
export function loadDrafts(): Record<string, string> {
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    if (raw === null) {
      return {};
    }
    const parsed: unknown = JSON.parse(raw);
    if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
      return {};
    }
    const drafts: Record<string, string> = {};
    for (const [sessionId, value] of Object.entries(parsed)) {
      if (typeof value === 'string' && value.length > 0) {
        drafts[sessionId] = value;
      }
    }
    return drafts;
  } catch {
    return {};
  }
}

export function saveDrafts(drafts: Record<string, string>): void {
  try {
    window.localStorage.setItem(STORAGE_KEY, JSON.stringify(drafts));
  } catch {
    // Storage can be unavailable (private mode, quota); drafts still work for
    // the session, they just will not survive a reload.
  }
}
