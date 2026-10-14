/**
 * @file catalog-editor-view.ts
 * @input Ids typed by the user, errors from the catalog API, the text of a file
 * @output The decisions the catalog editor shows: id problems, failure kinds, dirty state
 * @position Pure rules behind the catalog dialog; no React and no network
 */
import {CatalogRequestError} from './api.ts';
import type {CatalogKind} from './api.ts';

export type IdForm = 'category/name' | 'session';

export interface CatalogKindView {
  /** Singular noun for messages: "profile". */
  noun: string;
  /** Heading for the list: "Profiles". */
  plural: string;
  /** How a name is written, shown under the name field. */
  idHint: string;
  idForm: IdForm;
  /** What the delete confirmation tells the user besides the name. */
  deleteNote: string;
  /** The text a new file starts from. */
  template: string;
}

export const CATALOG_KIND_VIEWS: Record<CatalogKind, CatalogKindView> = {
  profiles: {
    noun: 'profile',
    plural: 'Profiles',
    idHint: 'Written <category>/<name>, for example coding/review.',
    idForm: 'category/name',
    deleteNote: 'The server refuses this while a session needs it.',
    template: '{\n  "schema_version": 1\n}\n',
  },
  models: {
    noun: 'model',
    plural: 'Models',
    idHint: 'Written <category>/<name>, for example openai/gpt-deep.',
    idForm: 'category/name',
    deleteNote: 'The server refuses this while a profile needs it.',
    template: '{\n  "schema_version": 1,\n  "provider": "openai",\n  "model": ""\n}\n',
  },
  sessions: {
    noun: 'session',
    plural: 'Sessions',
    idHint: 'Written <name>, or <root>:<name> when there is more than one session root.',
    idForm: 'session',
    deleteNote: 'A run in progress is stopped first.',
    template: '{\n  "schema_version": 1,\n  "cwd": ""\n}\n',
  },
  toolsets: {
    noun: 'toolset',
    plural: 'Toolsets',
    idHint: 'Written <category>/<name>, for example coding/review.',
    idForm: 'category/name',
    deleteNote: 'The server refuses this while a profile needs it.',
    template: '{\n  "schema_version": 1\n}\n',
  },
  mcp: {
    noun: 'MCP server',
    plural: 'MCP servers',
    idHint: 'Written <category>/<name>, for example jad/zvec-grep.',
    idForm: 'category/name',
    deleteNote: 'The server refuses this while a toolset needs it.',
    template: '{\n  "schema_version": 1,\n  "transport": {\n    "type": "stdio",\n    "command": ""\n  }\n}\n',
  },
};

export const STALE_FILE_MESSAGE =
  'This file changed on the server after you opened it. Reload it to see the current version; your edits are not saved.';

const IDENTIFIER = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;

/**
 * The server accepts `<category>/<name>` with letters, digits, '-', '_' and '.', and no
 * leading separator. The editor checks the same rule so a bad name never costs a round trip.
 */
export function newIdProblem(
  id: string,
  existing: readonly string[],
  noun = 'profile',
  form: IdForm = 'category/name',
): string | null {
  const trimmed = id.trim();
  if (form === 'session') return newSessionIdProblem(trimmed, existing);
  if (trimmed === '') return 'Enter a name written <category>/<name>.';
  const parts = trimmed.split('/');
  if (parts.length !== 2) return 'Write the name as <category>/<name>.';
  for (const part of parts) {
    if (part === '') return 'Write the name as <category>/<name>.';
    if (!IDENTIFIER.test(part)) {
      return `"${part}" may only hold letters, digits, '-', '_' and '.', and must start with a letter or digit.`;
    }
  }
  if (existing.includes(trimmed)) return `A ${noun} named "${trimmed}" already exists.`;
  return null;
}

const SESSION_ID = /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/;
const SESSION_ROOT_ID = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;

/** A session is `<name>`, or `<root>:<name>`; neither part may hold a slash or a second colon. */
function newSessionIdProblem(trimmed: string, existing: readonly string[]): string | null {
  if (trimmed === '') return 'Enter a name written <name> or <root>:<name>.';
  const parts = trimmed.split(':');
  if (parts.length > 2) return 'Write the name as <name> or <root>:<name>.';
  const name = parts[parts.length - 1];
  if (parts.length === 2 && !SESSION_ROOT_ID.test(parts[0])) {
    return `The root "${parts[0]}" may only hold letters, digits, '-', '_' and '.', and must start with a letter or digit.`;
  }
  if (!SESSION_ID.test(name)) {
    return `"${name}" may only hold letters, digits, '-', '_' and '.', must start with a letter or digit, and holds at most 128 characters.`;
  }
  if (existing.includes(trimmed)) return `A session named "${trimmed}" already exists.`;
  return null;
}

export type SaveFailureKind = 'stale' | 'exists' | 'invalid' | 'other';

export interface SaveFailure {
  kind: SaveFailureKind;
  message: string;
}

export function saveFailure(error: unknown, isCreate: boolean): SaveFailure {
  const message = error instanceof Error ? error.message : String(error);
  if (error instanceof CatalogRequestError) {
    if (error.status === 412) {
      return isCreate ? {kind: 'exists', message} : {kind: 'stale', message: STALE_FILE_MESSAGE};
    }
    if (error.status === 422) return {kind: 'invalid', message};
  }
  return {kind: 'other', message};
}

export type DeleteFailureKind = 'in-use' | 'stale' | 'other';

export interface DeleteFailure {
  kind: DeleteFailureKind;
  message: string;
}

export function deleteFailure(error: unknown): DeleteFailure {
  const message = error instanceof Error ? error.message : String(error);
  if (error instanceof CatalogRequestError) {
    if (error.status === 409) return {kind: 'in-use', message};
    if (error.status === 412) return {kind: 'stale', message: STALE_FILE_MESSAGE};
  }
  return {kind: 'other', message};
}

/** Files sort by id so a list does not reshuffle after a save. */
export function sortedIds(ids: readonly string[]): string[] {
  return [...ids].sort((a, b) => a.localeCompare(b));
}

export function isDirty(text: string, savedText: string): boolean {
  return text !== savedText;
}

export type EditorMode = 'none' | 'loading' | 'existing' | 'new';

/** A new file is unsaved work as soon as the name or the text differs from the kind's template. */
export function hasUnsavedWork(mode: EditorMode, text: string, savedText: string, newId: string, template: string): boolean {
  if (mode === 'none' || mode === 'loading') return false;
  if (mode === 'new') return isDirty(text, template) || newId.trim() !== '';
  return isDirty(text, savedText);
}

export interface SaveGate {
  mode: EditorMode;
  hasUnsavedWork: boolean;
  /** True when the problems on screen were computed for the text on screen. */
  isChecked: boolean;
  problemCount: number;
  isBusy: boolean;
  idProblem: string | null;
}

/** Create works on an untouched template once it has a name; Save needs a change. */
export function canSave(gate: SaveGate): boolean {
  if (gate.mode !== 'existing' && gate.mode !== 'new') return false;
  if (gate.isBusy || !gate.isChecked || gate.problemCount > 0) return false;
  if (gate.mode === 'new') return gate.idProblem === null;
  return gate.hasUnsavedWork;
}

/** CodeMirror stores line breaks as \n, so a CRLF file would otherwise open as already edited. */
export function normalizeLineBreaks(text: string): string {
  return text.replace(/\r\n?/g, '\n');
}
