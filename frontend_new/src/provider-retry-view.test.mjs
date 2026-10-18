import assert from 'node:assert/strict';
import test from 'node:test';

import {
  parseProviderRetryWait,
  providerRetryNotice,
  providerRetryReason,
  secondsLeft,
} from './provider-retry-view.ts';

const wire = {
  attempt: 2,
  max_retries: 5,
  status: 429,
  wait_ms: 4000,
  resume_at: '2026-01-01T00:00:04.000Z',
};
const at = ms => Date.parse('2026-01-01T00:00:00.000Z') + ms;

test('reads the wire form of a wait', () => {
  assert.deepEqual(parseProviderRetryWait(wire), {
    attempt: 2,
    maxRetries: 5,
    status: 429,
    waitMs: 4000,
    resumeAtMs: at(4000),
  });
});

test('a connection failure has a null status', () => {
  assert.equal(parseProviderRetryWait({...wire, status: null})?.status, null);
});

test('null and anything malformed clear the notice instead of leaving one on screen', () => {
  const bad = [
    null,
    undefined,
    'wait',
    [],
    {...wire, attempt: 0},
    {...wire, attempt: 6},
    {...wire, attempt: 1.5},
    {...wire, max_retries: 0},
    {...wire, status: 'x'},
    {...wire, status: 42},
    {...wire, wait_ms: -1},
    {...wire, wait_ms: '4000'},
    {...wire, resume_at: 'soon'},
    {...wire, resume_at: 7},
    {attempt: 1},
  ];
  for (const value of bad) {
    assert.equal(parseProviderRetryWait(value), undefined, JSON.stringify(value));
  }
});

test('seconds left rounds up, never goes below zero, and reaches zero only at the end', () => {
  const wait = parseProviderRetryWait(wire);
  assert.equal(secondsLeft(wait, at(0)), 4);
  assert.equal(secondsLeft(wait, at(100)), 4);
  assert.equal(secondsLeft(wait, at(3001)), 1);
  assert.equal(secondsLeft(wait, at(3999)), 1);
  assert.equal(secondsLeft(wait, at(4000)), 0);
  assert.equal(secondsLeft(wait, at(60_000)), 0);
});

test('the reason names the cause in plain words', () => {
  assert.match(providerRetryReason(429), /rate limiting.*429/);
  assert.match(providerRetryReason(503), /error \(503\)/);
  assert.match(providerRetryReason(408), /timed out/);
  assert.match(providerRetryReason(409), /conflict/);
  assert.match(providerRetryReason(null), /connection/);
  assert.match(providerRetryReason(418), /failed \(418\)/);
});

test('the notice gives the reason, the countdown and which retry it is', () => {
  const wait = parseProviderRetryWait(wire);
  assert.equal(
    providerRetryNotice(wait, at(1500)),
    'The provider is rate limiting requests (429). Retrying in 3 s (retry 2 of 5).',
  );
  assert.equal(
    providerRetryNotice(wait, at(4000)),
    'The provider is rate limiting requests (429). Retrying now (retry 2 of 5).',
  );
});
