/**
 * @file subagent-transcript-store.ts
 * @input A call's cache key and a function that fetches its subagent transcript
 * @output The latest transcript read for that key, one request per key at a time
 * @position Used by subagent-transcript-poll.ts (through components/SubagentTranscript.tsx)
 *
 * The store keeps the most recent `limit` transcripts, so hiding and reopening a transcript
 * (or coming back to the call) can show the last answer at once, and shares one request
 * between callers that ask for the same key while it is in flight. Every `refresh` asks the
 * server and replaces the entry: a child that is still running gives a longer answer each
 * time, so the key does not carry the step count. A failed fetch keeps what the entry held.
 * The store has no timer of its own; the poller decides when to ask.
 */
import type {WebUiSubagentTranscript} from './api.ts';

export interface SubagentTranscriptStore {
  /** The latest transcript for `key`, without starting a fetch. */
  peek(key: string): WebUiSubagentTranscript | undefined;
  /** Ask the server (one request per key at a time), keep the answer in place of the old one and return it. */
  refresh(key: string, fetch: () => Promise<WebUiSubagentTranscript>): Promise<WebUiSubagentTranscript>;
}

/**
 * The key of one call's transcript: session, profile and call id. The summary's step count
 * is not part of it (it is frozen when the call settles, while the child goes on); each
 * answer replaces the entry under the same key.
 */
export function subagentTranscriptKey(sessionRef: string, profile: string | undefined, callId: string): string {
  return JSON.stringify([sessionRef, profile ?? '', callId]);
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
    refresh(key, fetch) {
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
