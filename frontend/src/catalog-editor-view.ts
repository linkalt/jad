/**
 * @file catalog-editor-view.ts
 * @input Ids typed by the user, errors from the catalog API, the text of a file
 * @output The decisions the catalog editor shows: id problems, failure kinds, dirty state
 * @position Pure rules behind the catalog dialog; no React and no network
 */
import {CatalogRequestError} from './api.ts';

export const NEW_PROFILE_TEXT = '{\n  "schema_version": 1\n}\n';

export const STALE_FILE_MESSAGE =
  'This file changed on the server after you opened it. Reload it to see the current version; your edits are not saved.';

const IDENTIFIER = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;

/**
 * The server accepts `<category>/<name>` with letters, digits, '-', '_' and '.', and no
 * leading separator. The editor checks the same rule so a bad name never costs a round trip.
 */
export function newIdProblem(id: string, existing: readonly string[]): string | null {
  const trimmed = id.trim();
  if (trimmed === '') return 'Enter a name written <category>/<name>.';
  const parts = trimmed.split('/');
  if (parts.length !== 2) return 'Write the name as <category>/<name>.';
  for (const part of parts) {
    if (part === '') return 'Write the name as <category>/<name>.';
    if (!IDENTIFIER.test(part)) {
      return `"${part}" may only hold letters, digits, '-', '_' and '.', and must start with a letter or digit.`;
    }
  }
  if (existing.includes(trimmed)) return `A profile named "${trimmed}" already exists.`;
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

/** A new profile is unsaved work as soon as the name or the text differs from the empty template. */
export function hasUnsavedWork(mode: EditorMode, text: string, savedText: string, newId: string): boolean {
  if (mode === 'none' || mode === 'loading') return false;
  if (mode === 'new') return isDirty(text, NEW_PROFILE_TEXT) || newId.trim() !== '';
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
