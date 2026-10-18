import appConfig from './config.json';
import {
  promptLibrary as seedPrompts,
  type PromptCategory,
  type PromptTemplate,
} from './promptLibrary';

/**
 * Persistence for the prompt library.
 *
 * Prompts are user content, so they live in `localStorage` under a configured
 * key rather than in the module that ships the starter set. The seed list is
 * only a fallback: it is used the first time the app runs, and whenever the
 * stored value is missing or unreadable.
 */
const STORAGE_KEY = appConfig.prompt_library_storage_key;

export const PROMPT_CATEGORIES: readonly PromptCategory[] = [
  'Debug',
  'Explain',
  'Review',
  'Testing',
  'Writing',
];

function isPromptTemplate(value: unknown): value is PromptTemplate {
  if (typeof value !== 'object' || value === null) {
    return false;
  }
  const candidate = value as Record<string, unknown>;
  return (
    typeof candidate.id === 'string' &&
    candidate.id.length > 0 &&
    typeof candidate.title === 'string' &&
    typeof candidate.body === 'string' &&
    PROMPT_CATEGORIES.includes(candidate.category as PromptCategory)
  );
}

/**
 * Stored prompts, or the starter set.
 *
 * A stored empty array means the user deleted everything, so it is returned as
 * is — only a missing or malformed entry falls back to the seeds.
 */
export function loadPrompts(): PromptTemplate[] {
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    if (raw === null) {
      return seedLibrary();
    }
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) {
      return seedLibrary();
    }
    return parsed.filter(isPromptTemplate);
  } catch {
    return seedLibrary();
  }
}

export function savePrompts(prompts: readonly PromptTemplate[]): void {
  try {
    window.localStorage.setItem(STORAGE_KEY, JSON.stringify(prompts));
  } catch {
    // Storage can be unavailable (private mode, quota); the library still
    // works for the session, it just will not survive a reload.
  }
}

export function seedLibrary(): PromptTemplate[] {
  return seedPrompts.map(prompt => ({...prompt}));
}

/** Stable, readable id derived from the title, kept unique within the library. */
export function createPromptId(
  title: string,
  existing: readonly PromptTemplate[],
): string {
  const base =
    title
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, '-')
      .replace(/^-+|-+$/g, '')
      .slice(0, 40) || 'prompt';
  const taken = new Set(existing.map(prompt => prompt.id));
  if (!taken.has(base)) {
    return base;
  }
  let suffix = 2;
  while (taken.has(`${base}-${suffix}`)) {
    suffix += 1;
  }
  return `${base}-${suffix}`;
}
