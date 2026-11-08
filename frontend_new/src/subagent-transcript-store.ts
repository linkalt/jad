/**
 * @file subagent-transcript-store.ts
 * @input A call's cache key and a function that fetches its subagent transcript
 * @output The transcript, fetched at most once per key while it stays cached
 * @position Used by components/SubagentTranscript.tsx
 *
 * A subagent transcript is read only when the user opens it, so nothing here runs on
 * mount. The store keeps the most recent `limit` transcripts, so hiding and reopening a
 * transcript (or coming back to the call) does not fetch again, and shares one request
 * between callers that ask for the same key while it is in flight. A failed fetch is not
 * kept: the next `load` asks again. There is no polling and no prefetch.
 */
import type {WebUiSubagentTranscript} from './api.ts';

export interface SubagentTranscriptStore {
  /** The cached transcript for `key`, without starting a fetch. */
  peek(key: string): WebUiSubagentTranscript | undefined;
  /** The cached transcript, or the result of `fetch` (one request per key at a time). */
  load(key: string, fetch: () => Promise<WebUiSubagentTranscript>): Promise<WebUiSubagentTranscript>;
}

/**
 * The key of one call's transcript. The step count of the summary is part of it: a child
 * that has finished more steps since (a newer summary) is a different transcript, so a
 * cached partial one is never shown as current.
 */
export function subagentTranscriptKey(
  sessionRef: string,
  profile: string | undefined,
  callId: string,
  steps: number,
): string {
  return JSON.stringify([sessionRef, profile ?? '', callId, steps]);
}

export function createSubagentTranscriptStore(limit: number): SubagentTranscriptStore {
  const cache = new Map<string, WebUiSubagentTranscript>();
  const pending = new Map<string, Promise<WebUiSubagentTranscript>>();
  const keep = (key: string, transcript: WebUiSubagentTranscript) => {
    cache.delete(key);
    cache.set(key, transcript);
    while (cache.size > Math.max(1, limit)) {
      cache.delete(cache.keys().next().value as string);
    }
  };
  return {
    peek(key) {
      const transcript = cache.get(key);
      if (transcript !== undefined) {
        // Reading keeps it recent.
        cache.delete(key);
        cache.set(key, transcript);
      }
      return transcript;
    },
    load(key, fetch) {
      const cached = this.peek(key);
      if (cached !== undefined) return Promise.resolve(cached);
      const inFlight = pending.get(key);
      if (inFlight !== undefined) return inFlight;
      const request = fetch().then(
        transcript => {
          pending.delete(key);
          keep(key, transcript);
          return transcript;
        },
        error => {
          pending.delete(key);
          throw error;
        },
      );
      pending.set(key, request);
      return request;
    },
  };
}
