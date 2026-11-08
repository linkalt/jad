/**
 * @file web-ui-part-groups.ts
 * @input The parts of one message and its optional step ranges
 * @output Parts grouped into reasoning, tool, hosted-tool and content groups, the
 *   activity/content segments laid out from them, and the pairing of calls with results
 * @position Used by WebUiParts in components/TranscriptParts.tsx (the live
 *   conversation) and by read-only-transcript.ts (the read-only transcript)
 *
 * One grouping serves both views, so a read-only transcript groups tool runs exactly as
 * the conversation does: only adjacent tool parts of one step share a group, and text or
 * reasoning always ends the run.
 */
import type {WebUiHostedToolPart, WebUiMessageStep, WebUiPart} from './api.ts';
import {webUiStepAtPartIndex} from './web-ui-steps.ts';

export type WebUiToolCall = Extract<WebUiPart, {type: 'tool_call'}>;

export type WebUiToolResult = Extract<WebUiPart, {type: 'tool_result'}>;

export type WebUiReasoningPart = Extract<WebUiPart, {type: 'reasoning'}>;

export type WebUiContentPart = Extract<WebUiPart, {type: 'text'}>;

export type WebUiToolPart = WebUiToolCall | WebUiToolResult;

export type IndexedWebUiPart<T extends WebUiPart> = {part: T; index: number};

export type WebUiPartGroup =
  | {
      type: 'reasoning';
      item: IndexedWebUiPart<WebUiReasoningPart>;
    }
  | {
      type: 'tools';
      items: IndexedWebUiPart<WebUiToolPart>[];
    }
  | {
      type: 'hosted';
      item: IndexedWebUiPart<WebUiHostedToolPart>;
    }
  | {
      type: 'content';
      item: IndexedWebUiPart<WebUiContentPart>;
    };

export function isWebUiToolPart(part: WebUiPart): part is WebUiToolPart {
  return part.type === 'tool_call' || part.type === 'tool_result';
}

export function groupWebUiParts(
  parts: WebUiPart[],
  steps?: readonly WebUiMessageStep[],
): WebUiPartGroup[] {
  const groups: WebUiPartGroup[] = [];
  // Group only adjacent tool parts; reasoning and text always flush the tool run.
  let toolItems: IndexedWebUiPart<WebUiToolPart>[] = [];

  const flushTools = () => {
    if (toolItems.length > 0) {
      groups.push({type: 'tools', items: toolItems});
      toolItems = [];
    }
  };

  parts.forEach((part, index) => {
    if ((part.type === 'reasoning' || part.type === 'text') && part.text.trim().length === 0) {
      return;
    }
    if (isWebUiToolPart(part)) {
      const previousTool = toolItems[0];
      if (
        previousTool !== undefined &&
        webUiStepAtPartIndex(steps, previousTool.index)?.id !==
          webUiStepAtPartIndex(steps, index)?.id
      ) {
        flushTools();
      }
      toolItems.push({part, index});
      return;
    }

    flushTools();
    if (part.type === 'reasoning') {
      groups.push({type: 'reasoning', item: {part, index}});
      return;
    }
    if (part.type === 'hosted_tool') {
      groups.push({type: 'hosted', item: {part, index}});
      return;
    }
    groups.push({type: 'content', item: {part, index}});
  });
  flushTools();

  return groups;
}

export type WebUiActivityGroupPart = Extract<
  WebUiPartGroup,
  {type: 'reasoning' | 'tools' | 'hosted'}
>;

export type WebUiContentGroupPart = Extract<WebUiPartGroup, {type: 'content'}>;

export type IndexedWebUiActivityGroup = {group: WebUiActivityGroupPart; index: number};

export type WebUiSegment =
  | {type: 'activity'; items: IndexedWebUiActivityGroup[]}
  | {type: 'content'; item: {group: WebUiContentGroupPart; index: number}};

export function webUiGroupKey(group: WebUiPartGroup, index: number): string {
  if (group.type === 'tools') {
    return `tools-${group.items[0]?.index ?? index}`;
  }
  return `${group.type}-${group.item.index}`;
}

export function webUiGroupIndex(group: WebUiPartGroup): number {
  return group.type === 'tools' ? group.items[0]?.index ?? -1 : group.item.index;
}

/**
 * Reasoning rows and tool rows are the same Astryx call row, so consecutive
 * ones are laid out in a single tight stack: they keep the tool-to-tool row
 * rhythm instead of the wider block spacing used around Markdown text.
 */
export function segmentWebUiGroups(groups: WebUiPartGroup[]): WebUiSegment[] {
  const segments: WebUiSegment[] = [];
  let activity: IndexedWebUiActivityGroup[] = [];

  const flushActivity = () => {
    if (activity.length > 0) {
      segments.push({type: 'activity', items: activity});
      activity = [];
    }
  };

  groups.forEach((group, index) => {
    if (group.type === 'content') {
      flushActivity();
      segments.push({type: 'content', item: {group, index}});
      return;
    }
    activity.push({group, index});
  });
  flushActivity();

  return segments;
}

export interface PairedToolCall {
  call: WebUiToolCall;
  /** The first result after the call that carries its call id. */
  result: WebUiToolResult | undefined;
  /** Index of the call part in its message. */
  index: number;
}

/**
 * The calls of one tool group with the result each one got, and the results that answer
 * no call of the group. A result pairs with a call by call id and only when it follows the
 * call; a result with no earlier call of its id is an orphan.
 */
export function pairToolParts(items: readonly IndexedWebUiPart<WebUiToolPart>[]): {
  calls: PairedToolCall[];
  orphans: IndexedWebUiPart<WebUiToolResult>[];
} {
  const calls: PairedToolCall[] = [];
  const orphans: IndexedWebUiPart<WebUiToolResult>[] = [];
  const seen = new Set<string>();
  items.forEach(({part, index}, itemIndex) => {
    if (part.type === 'tool_call') {
      seen.add(part.callId);
      const result = items
        .slice(itemIndex + 1)
        .map(item => item.part)
        .find(
          (candidate): candidate is WebUiToolResult =>
            candidate.type === 'tool_result' && candidate.callId === part.callId,
        );
      calls.push({call: part, result, index});
    } else if (!seen.has(part.callId)) {
      orphans.push({part, index});
    }
  });
  return {calls, orphans};
}
