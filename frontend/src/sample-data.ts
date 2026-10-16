import {type ChatToolCallItem} from '@astryxdesign/core';
import {type MessageRecord, type ReasoningStep, type Session} from './app-types';

export const baseReasoning: Omit<ReasoningStep, 'status'>[] = [
  {
    label: 'Frame the request',
    detail: 'Separate the visual goal from implementation constraints.',
  },
  {
    label: 'Inspect the harness surface',
    detail: 'Locate the session, transcript, composer, and run inspector regions.',
  },
  {
    label: 'Draft a component plan',
    detail: 'Prefer Astryx layout, chat, status, and navigation primitives.',
  },
  {
    label: 'Check interaction paths',
    detail: 'Keep sending, stopping, switching sessions, and inspecting runs obvious.',
  },
  {
    label: 'Validate the mock output',
    detail: 'Confirm the response reads as a plausible agent run before shipping.',
  },
];

export const defaultTools: ChatToolCallItem[] = [
  {
    key: 'read-components',
    name: 'read_components',
    status: 'complete',
    target: 'Astryx component map',
    duration: '420ms',
    node: 'workspace',
  },
  {
    key: 'draft-layout',
    name: 'draft_layout',
    status: 'complete',
    target: 'Jad regions',
    duration: '680ms',
    node: 'planner',
    additions: 3,
  },
];

/**
 * Demo sessions. Live sessions are deliberately absent: they are owned by the
 * backend and arrive on every poll under their own names, so there is no
 * placeholder row standing in for a session that may not exist.
 */
export const initialSessions: Session[] = [
  {
    id: 'atlas',
    title: 'Atlas onboarding flow',
    time: '2 min ago',
    turns: 8,
    state: 'success',
    source: 'sample',
  },
  {
    id: 'settings',
    title: 'Settings surface audit',
    time: '18 min ago',
    turns: 5,
    state: 'warning',
    source: 'sample',
  },
  {
    id: 'empty',
    title: 'Empty state exploration',
    time: 'Yesterday',
    turns: 3,
    state: 'success',
    source: 'sample',
  },
];

export const initialThreads: Record<string, MessageRecord[]> = {
  atlas: [
    {
      id: 'atlas-system',
      sender: 'system',
      text: 'Run started · 12:41 PM',
      timestamp: '12:41 PM',
    },
    {
      id: 'atlas-user',
      sender: 'user',
      text: 'Build a settings screen for the new agent workspace. Keep the navigation compact and make tool activity easy to scan.',
      timestamp: '12:42 PM',
    },
    {
      id: 'atlas-assistant',
      sender: 'assistant',
      text: 'I mapped the request into a compact app shell with a **conversation-first** center column and an inspector that keeps the run trace close without competing with the response.\n\n- **Center:** conversation and composer\n- **Inspector:** reasoning and tool activity\n- **Navigation:** compact session history',
      timestamp: '12:43 PM',
      reasoning: baseReasoning.map(step => ({...step, status: 'complete'})),
      tools: defaultTools,
    },
  ],
  settings: [
    {
      id: 'settings-system',
      sender: 'system',
      text: 'Run started · 12:25 PM',
      timestamp: '12:25 PM',
    },
    {
      id: 'settings-user',
      sender: 'user',
      text: 'Audit the current settings surface and call out the highest-risk interaction gaps.',
      timestamp: '12:26 PM',
    },
    {
      id: 'settings-assistant',
      sender: 'assistant',
      text: 'The primary gap is **discoverability**: the settings groups are present, but the current hierarchy makes the next action hard to predict.\n\n> Recommendation: expose the next action before secondary configuration.',
      timestamp: '12:27 PM',
      reasoning: baseReasoning.map(step => ({...step, status: 'complete'})),
      tools: defaultTools,
    },
  ],
  empty: [
    {
      id: 'empty-system',
      sender: 'system',
      text: 'Run started · Yesterday',
      timestamp: 'Yesterday',
    },
    {
      id: 'empty-user',
      sender: 'user',
      text: 'Explore a calm first-run state for a workspace with no agent history.',
      timestamp: 'Yesterday',
    },
    {
      id: 'empty-assistant',
      sender: 'assistant',
      text: 'A useful empty state should explain what the agent can do, show one concrete starting prompt, and leave room for the user to bring their own task.\n\n`Start with a task` is a better first action than a blank transcript.',
      timestamp: 'Yesterday',
      reasoning: baseReasoning.map(step => ({...step, status: 'complete'})),
      tools: defaultTools,
    },
  ],
};

export function getTraceForSteps(
  steps: Omit<ReasoningStep, 'status'>[],
  progress: number,
): ReasoningStep[] {
  return steps.map((step, index) => ({
    ...step,
    status:
      index < progress
        ? 'complete'
        : index === progress
          ? 'running'
          : 'pending',
  }));
}
