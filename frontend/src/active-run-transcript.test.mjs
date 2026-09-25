import assert from 'node:assert/strict';
import test from 'node:test';

import {suppressAttachedRunTranscriptTail} from './active-run-transcript.ts';

test('uses the attached stream instead of earlier assistant steps from the active turn', () => {
  const messages = [
    {id: 'prior-user', sender: 'user'},
    {id: 'prior-assistant', sender: 'assistant'},
    {id: 'active-user', sender: 'user'},
    {id: 'completed-provider-step', sender: 'assistant', steps: [{id: 'step-1'}]},
    {id: 'another-provider-step', sender: 'assistant', steps: [{id: 'step-2'}]},
  ];

  assert.deepEqual(
    suppressAttachedRunTranscriptTail(messages, true),
    messages.slice(0, 3),
  );
});

test('keeps the full transcript when there is no attached live run', () => {
  const messages = [
    {id: 'user', sender: 'user'},
    {id: 'assistant', sender: 'assistant', status: 'complete'},
  ];

  assert.equal(suppressAttachedRunTranscriptTail(messages, false), messages);
});

test('does not suppress messages when no current user turn is present', () => {
  const messages = [{id: 'assistant', sender: 'assistant', status: 'complete'}];

  assert.equal(suppressAttachedRunTranscriptTail(messages, true), messages);
});
