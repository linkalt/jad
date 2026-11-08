/**
 * @file SubagentTranscript.tsx
 * @input A tool result's `subagent` snapshot
 * @output The right panel's subagent section: a "Show Transcript" button that reveals the
 *   "Subagent transcript" heading and the child's steps as a read-only conversation
 * @position Passed to ToolCallDetail as its children by ActivityPanel
 *
 * A static, one-shot view: completed steps only, nothing animates, polls or fetches. The
 * transcript is closed by default; the host keys this component by the open call, so every
 * open, and every switch to another call, starts closed. It is a caller of the shared
 * ReadOnlyTranscript, so tool calls and reasoning expand in place and open no panel. When
 * no step survives (none completed, only the child's summary, or a malformed snapshot) it
 * renders nothing, button included.
 */
import {useState} from 'react';
import {Button, Text, VStack} from '@astryxdesign/core';
import type {WebUiSubagentTranscript} from '../api';
import {subagentTranscriptMessages} from '../subagent-transcript';
import {ReadOnlyTranscript} from './ReadOnlyTranscript';

export function SubagentTranscript({subagent}: {subagent: WebUiSubagentTranscript}) {
  const [isOpen, setIsOpen] = useState(false);
  const messages = subagentTranscriptMessages(subagent);
  if (messages.length === 0) {
    return null;
  }
  return (
    <VStack gap={2} width="100%">
      <Button
        label={isOpen ? 'Hide Transcript' : 'Show Transcript'}
        variant="ghost"
        size="sm"
        aria-expanded={isOpen}
        onClick={() => setIsOpen(open => !open)}
      />
      {isOpen && (
        <VStack gap={2} width="100%">
          <Text type="supporting" color="secondary">
            {subagent.agent ? `Subagent transcript · ${subagent.agent}` : 'Subagent transcript'}
          </Text>
          <ReadOnlyTranscript messages={messages} />
        </VStack>
      )}
    </VStack>
  );
}
