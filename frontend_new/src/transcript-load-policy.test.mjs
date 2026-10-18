import assert from 'node:assert/strict';
import test from 'node:test';

import {shouldSkipTranscriptLoad} from './transcript-load-policy.ts';

test('hydrates history for a fresh page attached to an active run', () => {
  assert.equal(
    shouldSkipTranscriptLoad({
      sessionId: 'session-a',
      activeRunSessionId: 'session-a',
      attachedRunSessionId: 'session-a',
      transcriptHydrated: false,
    }),
    false,
  );
});

test('skips repeated transcript fetches after an attached page is hydrated', () => {
  assert.equal(
    shouldSkipTranscriptLoad({
      sessionId: 'session-a',
      activeRunSessionId: 'session-a',
      attachedRunSessionId: 'session-a',
      transcriptHydrated: true,
    }),
    true,
  );
});

test('keeps a client-started run from replacing its optimistic transcript', () => {
  assert.equal(
    shouldSkipTranscriptLoad({
      sessionId: 'session-a',
      activeRunSessionId: 'session-a',
      attachedRunSessionId: null,
      transcriptHydrated: false,
    }),
    true,
  );
});

test('does not block transcript loading for another session', () => {
  assert.equal(
    shouldSkipTranscriptLoad({
      sessionId: 'session-b',
      activeRunSessionId: 'session-a',
      attachedRunSessionId: 'session-a',
      transcriptHydrated: false,
    }),
    false,
  );
});
