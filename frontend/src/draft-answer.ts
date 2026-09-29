/**
 * @file draft-answer.ts
 * @input The dynamic JSON a tool proposed, and what the user typed into the drawer
 * @output Exports the row model, the draft an answer rebuilds, and its deadline labels
 * @position Pure model behind the composer's draft drawer
 *
 * A draft is any JSON object, so the form is derived rather than declared: every
 * scalar becomes one editable row, and a value a row cannot express — an array of
 * objects, a nested array — becomes one JSON row edited as text. Editing never
 * touches the proposal in place: the rows rebuild a copy, so a field the form does
 * not show (an empty array, an untouched nested object) keeps its exact value. A
 * row that no longer parses reports itself instead of throwing, because the user
 * has to see which field to fix.
 */

export type DraftFieldKind = 'string' | 'number' | 'boolean' | 'null' | 'json';

export interface DraftField {
  /** Stable row identity: the path, JSON-encoded so dots and brackets cannot collide. */
  key: string;
  /** Path from the draft root: `['steps', 0]`, `['scope', 'name']`. */
  path: (string | number)[];
  /** What the row is called: the path as the user reads it, `steps[1]`. */
  label: string;
  kind: DraftFieldKind;
  /** The proposal's own text, before any edit. */
  text: string;
}

export interface DraftFieldError {
  key: string;
  label: string;
  message: string;
}

export type DraftDecision = 'accept' | 'update' | 'decline';

/** The kinds a field the user adds may have; a new field is never `null`. */
export type DraftAddedFieldKind = 'string' | 'number' | 'boolean' | 'json';

/**
 * One field the user added to the draft — a key the proposal did not have.
 * Adding is a root-level key on purpose: nesting a new value into an existing
 * container is done by editing that container's own JSON row, so the added row
 * never has to spell a path.
 */
export interface DraftAddedField {
  /** Stable row identity; the drawer assigns one per opened row. */
  id: string;
  name: string;
  kind: DraftAddedFieldKind;
  text: string;
}

/** The kinds the drawer offers a new field, in the order it shows them. */
export const DRAFT_ADDED_FIELD_KINDS: ReadonlyArray<{id: DraftAddedFieldKind; label: string}> = [
  {id: 'string', label: 'Text'},
  {id: 'number', label: 'Number'},
  {id: 'boolean', label: 'True/false'},
  {id: 'json', label: 'JSON'},
];

const JSON_ROW_MESSAGE = 'must be valid JSON';

/** The editable rows of a draft, in the order the proposal lists them. */
export function draftFields(draft: unknown): DraftField[] {
  const fields: DraftField[] = [];
  visit(draft, []);
  return fields;

  function visit(value: unknown, path: (string | number)[]): void {
    if (isScalar(value)) {
      if (path.length === 0) {
        return;
      }
      fields.push(makeField(path, kindOf(value), scalarText(value)));
      return;
    }
    if (Array.isArray(value)) {
      if (value.length > 0 && value.every(isScalar)) {
        value.forEach((item, index) => {
          const childPath = [...path, index];
          fields.push(makeField(childPath, kindOf(item), scalarText(item)));
        });
        return;
      }
      if (path.length === 0) {
        return;
      }
      fields.push(makeField(path, 'json', prettyJson(value)));
      return;
    }
    const entries = Object.entries(value as Record<string, unknown>);
    for (const [key, child] of entries) {
      visit(child, [...path, key]);
    }
  }
}

/**
 * The draft the answer carries: the proposal with every edited row applied and
 * every field the user added written at the root. Rows the user did not touch
 * keep their value exactly, including containers the form shows as one JSON row.
 * An added row the user opened and left empty is not a change and is skipped.
 */
export function buildEditedDraft(
  draft: unknown,
  edits: Readonly<Record<string, string>>,
  added: readonly DraftAddedField[] = [],
): {value: unknown} | {error: DraftFieldError} {
  const value = structuredClone(draft) as unknown;
  for (const field of draftFields(draft)) {
    const text = edits[field.key];
    if (text === undefined || text === field.text) {
      continue;
    }
    const parsed = parseFieldText(field.kind, text);
    if ('error' in parsed) {
      return {error: {key: field.key, label: field.label, message: parsed.error}};
    }
    if (!setAtPath(value, field.path, parsed.value)) {
      return {error: {key: field.key, label: field.label, message: 'could not be applied'}};
    }
  }
  for (const field of added) {
    if (!hasAddedFields([field])) {
      continue;
    }
    const name = field.name.trim();
    if (name.length === 0) {
      return {error: {key: field.id, label: 'New field', message: 'needs a name'}};
    }
    if (name.includes('.') || name.includes('[') || name.includes(']')) {
      return {
        error: {
          key: field.id,
          label: name,
          message: 'must be a single key, without dots or brackets',
        },
      };
    }
    if (!isRecord(value)) {
      return {error: {key: field.id, label: name, message: 'cannot be added to this draft'}};
    }
    if (Object.prototype.hasOwnProperty.call(value, name)) {
      return {error: {key: field.id, label: name, message: 'already exists in the draft'}};
    }
    const parsed = parseFieldText(field.kind, field.text);
    if ('error' in parsed) {
      return {error: {key: field.id, label: name, message: parsed.error}};
    }
    value[name] = parsed.value;
  }
  return {value};
}

/** True while at least one row holds text the proposal did not. */
export function hasDraftEdits(
  fields: readonly DraftField[],
  edits: Readonly<Record<string, string>>,
): boolean {
  return fields.some((field) => {
    const text = edits[field.key];
    return text !== undefined && text !== field.text;
  });
}

/** True while an opened "new field" row holds anything the user typed. */
export function hasAddedFields(added: readonly DraftAddedField[]): boolean {
  return added.some((field) => field.name.trim().length > 0 || field.text.trim().length > 0);
}

/**
 * True when answering would carry an update rather than the proposal itself —
 * an edited row, or a field the user added. The drawer labels its primary action
 * from this, so an addition can never be dropped by the button next to the right
 * one.
 */
export function hasDraftChanges(
  fields: readonly DraftField[],
  edits: Readonly<Record<string, string>>,
  added: readonly DraftAddedField[],
): boolean {
  return hasDraftEdits(fields, edits) || hasAddedFields(added);
}

/**
 * The body an answer posts, or the reason the drawer cannot post it yet.
 *
 * `update` carries the edited draft because that is the version the run proceeds
 * with; `decline` requires the user's reason, the same rule the endpoint
 * enforces, so the drawer refuses before a request is wasted.
 */
export function reviewDraftAnswer(options: {
  draft: unknown;
  fields: readonly DraftField[];
  edits: Readonly<Record<string, string>>;
  added: readonly DraftAddedField[];
  decision: DraftDecision;
  reason: string;
  reasonMaxLength: number;
}): {body: {decision: DraftDecision; draft?: unknown; reason?: string}} | {error: string} {
  if (options.decision === 'decline') {
    const reason = options.reason.trim();
    if (reason.length === 0) {
      return {error: 'Say why you are declining.'};
    }
    if (reason.length > options.reasonMaxLength) {
      return {error: `Keep the reason under ${options.reasonMaxLength} characters.`};
    }
    return {body: {decision: 'decline', reason}};
  }
  if (options.decision === 'accept') {
    return {body: {decision: 'accept'}};
  }
  const edited = buildEditedDraft(options.draft, options.edits, options.added);
  if ('error' in edited) {
    return {error: `${edited.error.label} ${edited.error.message}.`};
  }
  return {body: {decision: 'update', draft: edited.value}};
}

/** How long the run keeps waiting, as the drawer's own line of text. */
export function draftDeadlineLabel(expiresAt: string, now: number): string {
  const deadline = Date.parse(expiresAt);
  if (Number.isNaN(deadline)) {
    return '';
  }
  const remainingMs = deadline - now;
  if (remainingMs <= 0) {
    return 'the wait is over';
  }
  const minutes = Math.floor(remainingMs / 60_000);
  if (minutes >= 60) {
    const hours = Math.floor(minutes / 60);
    return `${hours} ${hours === 1 ? 'hour' : 'hours'} left`;
  }
  if (minutes >= 1) {
    return `${minutes} ${minutes === 1 ? 'minute' : 'minutes'} left`;
  }
  return 'less than a minute left';
}

/** How much one extension adds, as the drawer's button label: `+5 min`, `+30 sec`. */
export function draftExtendLabel(extendMs: number): string {
  if (!Number.isFinite(extendMs) || extendMs <= 0) {
    return '';
  }
  const minutes = Math.floor(extendMs / 60_000);
  if (minutes >= 1) {
    return `+${minutes} min`;
  }
  return `+${Math.round(extendMs / 1000)} sec`;
}

/**
 * True while extending can still push the deadline: the wait has not ended and
 * the question is not already at the longest wait the session allows.
 */
export function canExtendDraft(expiresAt: string, maxExpiresAt: string, now: number): boolean {
  const deadline = Date.parse(expiresAt);
  const ceiling = Date.parse(maxExpiresAt);
  if (Number.isNaN(deadline) || Number.isNaN(ceiling)) {
    return false;
  }
  return now < deadline && deadline < ceiling;
}

function makeField(path: (string | number)[], kind: DraftFieldKind, text: string): DraftField {
  return {key: JSON.stringify(path), path, label: labelFor(path), kind, text};
}

function labelFor(path: readonly (string | number)[]): string {
  let label = '';
  for (const part of path) {
    if (typeof part === 'number') {
      label += `[${part + 1}]`;
      continue;
    }
    label += label.length === 0 ? part : `.${part}`;
  }
  return label;
}

function kindOf(value: unknown): DraftFieldKind {
  if (value === null) {
    return 'null';
  }
  switch (typeof value) {
    case 'number':
      return 'number';
    case 'boolean':
      return 'boolean';
    default:
      return 'string';
  }
}

function scalarText(value: unknown): string {
  if (value === null) {
    return '';
  }
  if (typeof value === 'string') {
    return value;
  }
  return String(value);
}

function prettyJson(value: unknown): string {
  try {
    return JSON.stringify(value, null, 2) ?? '';
  } catch {
    return '';
  }
}

function isScalar(value: unknown): value is null | boolean | number | string {
  return (
    value === null ||
    typeof value === 'boolean' ||
    typeof value === 'number' ||
    typeof value === 'string'
  );
}

function parseFieldText(kind: DraftFieldKind, text: string): {value: unknown} | {error: string} {
  switch (kind) {
    case 'string':
      return {value: text};
    case 'number': {
      const trimmed = text.trim();
      const parsed = Number(trimmed);
      return trimmed.length > 0 && Number.isFinite(parsed)
        ? {value: parsed}
        : {error: 'must be a number'};
    }
    case 'boolean': {
      const trimmed = text.trim().toLowerCase();
      if (trimmed === 'true') {
        return {value: true};
      }
      return trimmed === 'false' ? {value: false} : {error: 'must be true or false'};
    }
    case 'null': {
      const trimmed = text.trim().toLowerCase();
      return trimmed === '' || trimmed === 'null' ? {value: null} : {error: 'must stay empty'};
    }
    default: {
      try {
        return {value: JSON.parse(text) as unknown};
      } catch {
        return {error: JSON_ROW_MESSAGE};
      }
    }
  }
}

/** Writes `value` at `path` in place; false when the path no longer exists. */
function setAtPath(root: unknown, path: readonly (string | number)[], value: unknown): boolean {
  if (path.length === 0) {
    return false;
  }
  let cursor: unknown = root;
  for (const part of path.slice(0, -1)) {
    if (Array.isArray(cursor) && typeof part === 'number') {
      cursor = cursor[part];
      continue;
    }
    if (isRecord(cursor) && typeof part === 'string') {
      cursor = cursor[part];
      continue;
    }
    return false;
  }
  const last = path[path.length - 1]!;
  if (Array.isArray(cursor) && typeof last === 'number') {
    cursor[last] = value;
    return true;
  }
  if (isRecord(cursor) && typeof last === 'string') {
    cursor[last] = value;
    return true;
  }
  return false;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}
