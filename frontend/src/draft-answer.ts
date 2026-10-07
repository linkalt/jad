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

export type DraftItemKind = 'string' | 'number' | 'boolean' | 'null' | 'json' | 'object';
export type DraftFieldKind = DraftItemKind | 'list';

/**
 * The form of an array entry that is an object: its own proposal and the edits
 * made to it, so each key is an input like a root field. It nests, because a
 * key of that object may itself be an array of objects.
 */
export interface DraftItemForm {
  base: Record<string, unknown>;
  edits: Readonly<Record<string, string>>;
  lists: DraftListEdits;
}

/**
 * One entry of an array field. An array is one field, not one row per index, so
 * its entries can be added, removed and reordered; each entry keeps the type the
 * proposal gave it. An object entry with keys carries a `form` (one input per
 * key); an empty object or a nested array reads as one JSON entry.
 */
export interface DraftListItem {
  /** Stable identity across reorders: the original index, or `item-N` for an added entry. */
  id: string;
  kind: DraftItemKind;
  text: string;
  /** True for an entry the user added, which may still be retyped. */
  added?: boolean;
  /** Present for `object` entries only. */
  form?: DraftItemForm;
}

export interface DraftField {
  /** Stable row identity: the path, JSON-encoded so dots and brackets cannot collide. */
  key: string;
  /** Path from the draft root: `['scope', 'name']`, `['scope', 'steps']`. */
  path: (string | number)[];
  /** What the row is called: the path as the user reads it, `scope.steps`. */
  label: string;
  kind: DraftFieldKind;
  /** The proposal's own text, before any edit. */
  text: string;
  /** The entries of an array field, as the proposal had them. */
  items?: DraftListItem[];
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
      if (path.length === 0) {
        return;
      }
      fields.push({
        ...makeField(path, 'list', prettyJson(value)),
        items: value.map((item, index) => makeItem(String(index), item)),
      });
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
  lists: DraftListEdits = {},
): {value: unknown} | {error: DraftFieldError} {
  const value = structuredClone(draft) as unknown;
  for (const field of draftFields(draft)) {
    if (field.kind === 'list') {
      const items = lists[field.key];
      if (items === undefined || !isListEdited(field, items)) {
        continue;
      }
      const values: unknown[] = [];
      for (const [index, item] of items.entries()) {
        const parsed = parseListItem(item);
        if ('error' in parsed) {
          return {
            error: {
              key: field.key,
              label: `${field.label} item ${index + 1}${parsed.label === undefined ? '' : ` ${parsed.label}`}`,
              message: parsed.error,
            },
          };
        }
        values.push(parsed.value);
      }
      if (!setAtPath(value, field.path, values)) {
        return {error: {key: field.key, label: field.label, message: 'could not be applied'}};
      }
      continue;
    }
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
    Object.defineProperty(value, name, {
      value: parsed.value,
      enumerable: true,
      configurable: true,
      writable: true,
    });
  }
  return {value};
}

/** The entries the user has made of each array field, by field key. */
export type DraftListEdits = Readonly<Record<string, readonly DraftListItem[]>>;

/** True while at least one row or array holds something the proposal did not. */
export function hasDraftEdits(
  fields: readonly DraftField[],
  edits: Readonly<Record<string, string>>,
  lists: DraftListEdits = {},
): boolean {
  return fields.some((field) => {
    if (field.kind === 'list') {
      const items = lists[field.key];
      return items !== undefined && isListEdited(field, items);
    }
    const text = edits[field.key];
    return text !== undefined && text !== field.text;
  });
}

/** The entries an array field shows: the user's, or the proposal's own. */
export function listItemsOf(
  field: DraftField,
  lists: DraftListEdits,
): readonly DraftListItem[] {
  return lists[field.key] ?? field.items ?? [];
}

/** True when entries were added, removed, reordered, retyped or their text changed. */
export function isListEdited(field: DraftField, items: readonly DraftListItem[]): boolean {
  const original = field.items ?? [];
  return (
    items.length !== original.length ||
    items.some((item, index) => {
      const first = original[index];
      return (
        first === undefined ||
        item.id !== first.id ||
        item.kind !== first.kind ||
        item.text !== first.text ||
        (item.form !== undefined && isFormEdited(item.form))
      );
    })
  );
}

/** True when any input inside an object entry differs from its proposal. */
function isFormEdited(form: DraftItemForm): boolean {
  return hasDraftEdits(draftFields(form.base), form.edits, form.lists);
}

/** One entry's value, or the rule it broke; `label` names the input inside an object entry. */
function parseListItem(item: DraftListItem): {value: unknown} | {error: string; label?: string} {
  if (item.kind !== 'object' || item.form === undefined) {
    return parseFieldText(item.kind, item.text);
  }
  const built = buildEditedDraft(item.form.base, item.form.edits, [], item.form.lists);
  if ('error' in built) {
    return {error: built.error.message, label: built.error.label};
  }
  return {value: built.value};
}

/** The shape of a value with nothing filled in: what a new object entry starts from. */
function blankLike(value: unknown): unknown {
  if (value === null || typeof value === 'boolean') {
    return value === null ? null : false;
  }
  if (typeof value === 'number') {
    return 0;
  }
  if (typeof value === 'string') {
    return '';
  }
  if (Array.isArray(value)) {
    return [];
  }
  const blank: Record<string, unknown> = {};
  for (const [key, child] of Object.entries(value as Record<string, unknown>)) {
    blank[key] = blankLike(child);
  }
  return blank;
}

/** Replaces part of an object entry's form; any other entry is returned as it was. */
export function updateListItemForm(
  items: readonly DraftListItem[],
  index: number,
  patch: Partial<DraftItemForm>,
): DraftListItem[] {
  return items.map((item, position) =>
    position === index && item.form !== undefined
      ? {...item, form: {...item.form, ...patch}}
      : item,
  );
}

/** Swaps an entry with its neighbour; out of range returns the list unchanged. */
export function moveListItem(
  items: readonly DraftListItem[],
  index: number,
  offset: -1 | 1,
): DraftListItem[] {
  const target = index + offset;
  if (index < 0 || index >= items.length || target < 0 || target >= items.length) {
    return [...items];
  }
  const next = [...items];
  [next[index], next[target]] = [next[target]!, next[index]!];
  return next;
}

export function removeListItem(items: readonly DraftListItem[], index: number): DraftListItem[] {
  return items.filter((_, position) => position !== index);
}

/**
 * Appends an empty entry. It takes the type of the entry before it, so a list of
 * numbers grows numbers; an empty list or a list of nulls starts with text. After
 * an object entry it appends an object with the same keys, all blank.
 */
export function addListItem(
  items: readonly DraftListItem[],
  id: string,
  kind?: DraftAddedFieldKind,
): DraftListItem[] {
  const last = items[items.length - 1];
  if (kind === undefined && last?.kind === 'object' && last.form !== undefined) {
    const base = blankLike(last.form.base) as Record<string, unknown>;
    return [
      ...items,
      {id, kind: 'object', text: '', added: true, form: {base, edits: {}, lists: {}}},
    ];
  }
  const inherited: DraftAddedFieldKind =
    last === undefined || last.kind === 'null' || last.kind === 'object' ? 'string' : last.kind;
  return [...items, {id, kind: kind ?? inherited, text: '', added: true}];
}

/** Retypes an added entry; its text is kept so a mis-click costs nothing. */
export function retypeListItem(
  items: readonly DraftListItem[],
  index: number,
  kind: DraftAddedFieldKind,
): DraftListItem[] {
  return items.map((item, position) =>
    position === index && item.added === true ? {...item, kind} : item,
  );
}

export function editListItem(
  items: readonly DraftListItem[],
  index: number,
  text: string,
): DraftListItem[] {
  return items.map((item, position) => (position === index ? {...item, text} : item));
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
  lists: DraftListEdits = {},
): boolean {
  return hasDraftEdits(fields, edits, lists) || hasAddedFields(added);
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
  lists?: DraftListEdits;
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
  const edited = buildEditedDraft(options.draft, options.edits, options.added, options.lists);
  if ('error' in edited) {
    return {error: `${edited.error.label} ${edited.error.message}.`};
  }
  return {body: {decision: 'update', draft: edited.value}};
}

/** Parses the full-JSON editor's value and enforces the dynamic object envelope. */
export function reviewDraftJsonAnswer(
  text: string,
): {body: {decision: 'update'; draft: Record<string, unknown>}} | {error: string} {
  let value: unknown;
  try {
    value = JSON.parse(text) as unknown;
  } catch {
    return {error: 'Draft must be valid JSON.'};
  }
  if (!isRecord(value)) {
    return {error: 'Draft must be a JSON object.'};
  }
  return {body: {decision: 'update', draft: value}};
}

/** True when the raw editor contains a semantic object change or invalid text. */
export function hasJsonDraftChanges(text: string, original: unknown): boolean {
  const reviewed = reviewDraftJsonAnswer(text);
  return 'error' in reviewed || JSON.stringify(reviewed.body.draft) !== JSON.stringify(original);
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

function makeItem(id: string, value: unknown): DraftListItem {
  if (isScalar(value)) {
    return {id, kind: kindOf(value), text: scalarText(value)};
  }
  if (isRecord(value) && Object.keys(value).length > 0) {
    return {
      id,
      kind: 'object',
      text: prettyJson(value),
      form: {base: value, edits: {}, lists: {}},
    };
  }
  return {id, kind: 'json', text: prettyJson(value)};
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

function kindOf(value: unknown): DraftItemKind {
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

function parseFieldText(kind: DraftItemKind, text: string): {value: unknown} | {error: string} {
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
