/**
 * @file SubagentTranscript.tsx
 * @input A tool result's `subagent` snapshot
 * @output The "Subagent transcript" section of the right panel: the child's completed steps
 *   as a read-only conversation
 * @position Passed to ToolCallDetail as its children by ActivityPanel
 *
 * A static, one-shot view: completed steps only, nothing animates, polls or fetches. It is
 * a caller of the shared ReadOnlyTranscript, so tool calls and reasoning expand in place
 * and open no panel. A snapshot with no completed step renders nothing, and a malformed
 * one is treated as absent.
 */
import {Text, VStack} from '@astryxdesign/core';
import type {WebUiSubagentTranscript} from '../api';
import {subagentTranscriptMessages} from '../subagent-transcript';
import {ReadOnlyTranscript} from './ReadOnlyTranscript';

export function SubagentTranscript({subagent}: {subagent: WebUiSubagentTranscript}) {
  const messages = subagentTranscriptMessages(subagent);
  if (messages.length === 0) {
    return null;
  }
  return (
    <VStack gap={2} width="100%">
      <Text type="supporting" color="secondary">
        {subagent.agent ? `Subagent transcript · ${subagent.agent}` : 'Subagent transcript'}
      </Text>
      <ReadOnlyTranscript messages={messages} />
    </VStack>
  );
}
