export type PromptCategory = 'Debug' | 'Explain' | 'Review' | 'Testing' | 'Writing';

export type PromptTemplate = {
  id: string;
  title: string;
  category: PromptCategory;
  body: string;
};

/**
 * Seed content for the prompt library view.
 *
 * The data is kept out of the panel component on purpose: the floating panel is
 * a view other datasets will share, so the panel renders whatever list it is
 * handed rather than owning the content itself.
 */
export const promptLibrary: PromptTemplate[] = [
  {
    id: 'inspect-failure',
    title: 'Inspect a failing run',
    category: 'Debug',
    body: 'Summarise why the last run failed, list the tool calls involved in order, and propose the smallest change that would fix it.',
  },
  {
    id: 'explain-trace',
    title: 'Explain the reasoning trace',
    category: 'Explain',
    body: 'Walk through the reasoning steps in order, and flag any step that does not follow from the request or the previous step.',
  },
  {
    id: 'summarise-session',
    title: 'Summarise this session',
    category: 'Writing',
    body: 'Write a short handover note covering what was asked, what changed, and what is still open.',
  },
  {
    id: 'audit-harness',
    title: 'Audit the harness contract',
    category: 'Review',
    body: 'Check the run against the documented harness contract and list any part of the response that is simulated rather than live.',
  },
  {
    id: 'turn-into-test',
    title: 'Turn this transcript into a test',
    category: 'Testing',
    body: 'Convert the conversation into a deterministic test case: the input, the expected tool calls, and the assertions worth keeping.',
  },
  {
    id: 'tighten-prompt',
    title: 'Tighten a draft prompt',
    category: 'Writing',
    body: 'Rewrite the prompt below so it states the goal, the constraints, and the expected output format, without adding new requirements.',
  },
];
