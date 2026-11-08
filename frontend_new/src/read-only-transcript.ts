/**
 * @file read-only-transcript.ts
 * @input Read-only messages: a role, the parts of the message and optional step ranges
 * @output The rows a read-only transcript shows, in order, with their calls paired to
 *   their results
 * @position Used by components/ReadOnlyTranscript.tsx; the subagent snapshot maps onto it
 *   in subagent-transcript.ts, and a transcript reader will pass whole transcripts
 *
 * Public API (one place, shared by every read-only view):
 * - `ReadOnlyMessage`: one message to show. `key` is unique within the list handed to
 *   the renderer; `role` is 'user' (a right-aligned bubble of its text parts) or
 *   'assistant' (text, reasoning, tool calls); `parts` are the parts the conversation
 *   already uses (`WebUiPart`); `steps` are the message's step ranges, optional, and only
 *   decide where a run of tool calls ends: tool parts of different steps never share a
 *   group, as in the conversation.
 * - `readOnlySegments(message)`: the rows of one message, with text apart and reasoning,
 *   tool and hosted-tool rows stacked tight, as the conversation lays them out.
 *
 * Everything here is a completed snapshot. There is no streaming state: a call without a
 * result in the list is shown settled, never running, and nothing in a row refers to a
 * panel, a fork or a checkpoint. Images of tool results are not shown.
 */
import type {WebUiHostedToolPart, WebUiMessageStep, WebUiPart} from './api.ts';
import {
  type PairedToolCall,
  type WebUiContentPart,
  type WebUiReasoningPart,
  type WebUiToolResult,
  groupWebUiParts,
  pairToolParts,
  segmentWebUiGroups,
} from './web-ui-part-groups.ts';

export interface ReadOnlyMessage {
  key: string;
  role: 'user' | 'assistant';
  parts: WebUiPart[];
  steps?: readonly WebUiMessageStep[];
}

/** One call of a tool row, with the result that answered it, if the list has one. */
export interface ReadOnlyToolCall extends PairedToolCall {
  /** Unique within the renderer; scoped by the message so two messages never collide. */
  key: string;
}

export type ReadOnlyRow =
  | {type: 'text'; key: string; part: WebUiContentPart}
  | {type: 'reasoning'; key: string; part: WebUiReasoningPart}
  | {
      type: 'tools';
      key: string;
      calls: ReadOnlyToolCall[];
      /** Results with no call in their group; shown as plain result blocks after the calls. */
      orphans: {key: string; result: WebUiToolResult}[];
    }
  | {type: 'hosted'; key: string; part: WebUiHostedToolPart};

export type ReadOnlySegment =
  | {type: 'content'; row: Extract<ReadOnlyRow, {type: 'text'}>}
  | {type: 'activity'; rows: Exclude<ReadOnlyRow, {type: 'text'}>[]};

/** The rows of one message, grouped and paired the way the conversation groups them. */
export function readOnlySegments(message: ReadOnlyMessage): ReadOnlySegment[] {
  const scope = message.key;
  return segmentWebUiGroups(groupWebUiParts(message.parts, message.steps)).map(
    (segment): ReadOnlySegment => {
      if (segment.type === 'content') {
        const {item} = segment.item.group;
        return {type: 'content', row: {type: 'text', key: `${scope}:text-${item.index}`, part: item.part}};
      }
      return {
        type: 'activity',
        rows: segment.items.map(({group}): Exclude<ReadOnlyRow, {type: 'text'}> => {
          if (group.type === 'reasoning') {
            return {type: 'reasoning', key: `${scope}:reasoning-${group.item.index}`, part: group.item.part};
          }
          if (group.type === 'hosted') {
            return {type: 'hosted', key: `${scope}:hosted-${group.item.index}`, part: group.item.part};
          }
          const {calls, orphans} = pairToolParts(group.items);
          const first = group.items[0]?.index ?? 0;
          return {
            type: 'tools',
            key: `${scope}:tools-${first}`,
            calls: calls.map(call => ({...call, key: `${scope}:call-${call.index}`})),
            orphans: orphans.map(({part, index}) => ({key: `${scope}:result-${index}`, result: part})),
          };
        }),
      };
    },
  );
}
