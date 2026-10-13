/**
 * @file catalog-validation.ts
 * @input The sample/schemes JSON schemas and the text of one catalog file
 * @output Problems with character ranges: JSON syntax first, then schema violations
 * @position The client-side check behind the catalog editor; the server's loader stays the authority
 */
import Ajv2020 from 'ajv/dist/2020.js';
import type {ErrorObject, ValidateFunction} from 'ajv/dist/2020.js';

import {locatePointer, problemRange, syntaxProblem, type DocumentProblem} from './json-document.ts';

/** The catalog kinds the editor handles, and the scheme that describes each. */
export const CATALOG_SCHEMES = {
  profiles: 'profile.schema.json',
  models: 'model.schema.json',
} as const;

export type CatalogKind = keyof typeof CATALOG_SCHEMES;

export interface CatalogValidator {
  validate(kind: CatalogKind, text: string): DocumentProblem[];
}

function describe(error: ErrorObject): string {
  const allowed = (error.params as {allowedValues?: unknown}).allowedValues;
  if (error.keyword === 'enum' && Array.isArray(allowed)) {
    return `${error.message ?? 'must be one of'}: ${allowed.map(value => JSON.stringify(value)).join(', ')}`;
  }
  if (error.keyword === 'const') {
    return `must be ${JSON.stringify((error.params as {allowedValue?: unknown}).allowedValue)}`;
  }
  if (error.keyword === 'additionalProperties') {
    return `unknown key "${(error.params as {additionalProperty: string}).additionalProperty}"`;
  }
  if (error.keyword === 'required') {
    return `missing required key "${(error.params as {missingProperty: string}).missingProperty}"`;
  }
  return error.message ?? 'is not valid';
}

function escapePointer(segment: string): string {
  return segment.replace(/~/g, '~0').replace(/\//g, '~1');
}

/** The pointer a schema error should be shown at, and whether the key is the better anchor. */
function anchor(error: ErrorObject): {pointer: string; preferKey: boolean} {
  if (error.keyword === 'additionalProperties') {
    const key = (error.params as {additionalProperty: string}).additionalProperty;
    return {pointer: `${error.instancePath}/${escapePointer(key)}`, preferKey: true};
  }
  if (error.propertyName !== undefined) {
    return {pointer: `${error.instancePath}/${escapePointer(error.propertyName)}`, preferKey: true};
  }
  return {pointer: error.instancePath, preferKey: error.keyword !== 'required'};
}

/**
 * All schemes are registered together because a profile, a session and a toolset
 * point into setting.schema.json. Formats are not checked: the schemes use "uri"
 * for documentation, and the server is what accepts or refuses a value.
 */
export function createCatalogValidator(schemes: Readonly<Record<string, unknown>>): CatalogValidator {
  const ajv = new Ajv2020({allErrors: true, strict: false, validateFormats: false});
  for (const [file, schema] of Object.entries(schemes)) {
    ajv.addSchema(schema as object, file);
  }
  const compiled = new Map<CatalogKind, ValidateFunction>();
  const validatorFor = (kind: CatalogKind): ValidateFunction => {
    const cached = compiled.get(kind);
    if (cached) return cached;
    const check = ajv.getSchema(CATALOG_SCHEMES[kind]);
    if (!check) throw new Error(`the scheme ${CATALOG_SCHEMES[kind]} is missing`);
    compiled.set(kind, check);
    return check;
  };

  return {
    validate(kind, text) {
      const syntax = syntaxProblem(text);
      if (syntax) return [syntax];
      const check = validatorFor(kind);
      if (check(JSON.parse(text))) return [];
      const problems: DocumentProblem[] = [];
      const seen = new Set<string>();
      for (const error of check.errors ?? []) {
        // The wrapper adds nothing: the failing key's own error carries the detail.
        if (error.keyword === 'propertyNames') continue;
        const {pointer, preferKey} = anchor(error);
        const target = locatePointer(text, pointer) ?? locatePointer(text, '');
        if (!target) continue;
        const range = problemRange(target, text.length, preferKey);
        const place = error.instancePath === '' ? 'document' : error.instancePath;
        const subject = error.propertyName === undefined ? '' : ` key "${error.propertyName}"`;
        const message = `${place}${subject}: ${describe(error)}`;
        const key = `${range.from}:${range.to}:${message}`;
        if (seen.has(key)) continue;
        seen.add(key);
        problems.push({...range, message});
      }
      return problems.sort((a, b) => a.from - b.from);
    },
  };
}
