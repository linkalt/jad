import assert from 'node:assert/strict';
import test from 'node:test';

import {latestToolCallMessage, latestToolsMessage} from './transcript-facts.ts';

test('finds the newest assistant message with a tool call', () => {
  const older = {id: 'a', sender: 'assistant', parts: [{type: 'tool_call'}]};
  const newer = {id: 'b', sender: 'assistant', parts: [{type: 'tool_call'}]};
  const messages = [older, {id: 'u', sender: 'user'}, newer, {id: 'c', sender: 'assistant', parts: [{type: 'text'}]}];

  assert.equal(latestToolCallMessage(messages), newer);
});

test('ignores user messages and messages without a tool call', () => {
  const messages = [
    {id: 'u', sender: 'user', parts: [{type: 'tool_call'}]},
    {id: 'a', sender: 'assistant', parts: [{type: 'text'}]},
    {id: 'b', sender: 'assistant'},
  ];

  assert.equal(latestToolCallMessage(messages), undefined);
  assert.equal(latestToolCallMessage([]), undefined);
});

test('finds the newest assistant message that carries a tool list', () => {
  const older = {id: 'a', sender: 'assistant', tools: [{name: 'read'}]};
  const newer = {id: 'c', sender: 'assistant', tools: [{name: 'write'}]};
  const messages = [{id: 'u', sender: 'user', tools: [{name: 'x'}]}, older, newer, {id: 'b', sender: 'assistant'}];

  assert.equal(latestToolsMessage(messages), newer);
  assert.equal(latestToolsMessage([{id: 'a', sender: 'assistant'}]), undefined);
});
