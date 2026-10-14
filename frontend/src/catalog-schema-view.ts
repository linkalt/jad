/**
 * @file catalog-schema-view.ts
 * @input One sample/schemes JSON schema
 * @output The top-level fields it allows, each with a type label, its default and its description
 * @position Pure rules behind the catalog editor's Schema tab; no React and no network
 */
export interface SchemaField {
  name: string;
  isRequired: boolean;
  /** "string", "integer or null", `1` for a constant; empty when the schema gives none (a $ref or oneOf). */
  type: string;
  defaultValue?: string;
  description: string;
}

function typeLabel(property: Record<string, unknown>): string {
  const {type, enum: allowed} = property;
  if (typeof type === 'string') return type;
  if (Array.isArray(type)) return type.filter(item => typeof item === 'string').join(' or ');
  if ('const' in property) return JSON.stringify(property.const);
  if (Array.isArray(allowed)) return allowed.map(value => JSON.stringify(value)).join(' | ');
  return '';
}

export function schemaFields(schema: unknown): SchemaField[] {
  if (typeof schema !== 'object' || schema === null) return [];
  const {properties, required} = schema as {properties?: unknown; required?: unknown};
  if (typeof properties !== 'object' || properties === null) return [];
  const requiredNames = new Set(Array.isArray(required) ? required.filter(item => typeof item === 'string') : []);
  return Object.entries(properties as Record<string, unknown>).map(([name, value]) => {
    const property = typeof value === 'object' && value !== null ? (value as Record<string, unknown>) : {};
    const field: SchemaField = {
      name,
      isRequired: requiredNames.has(name),
      type: typeLabel(property),
      description: typeof property.description === 'string' ? property.description : '',
    };
    if ('default' in property) field.defaultValue = JSON.stringify(property.default);
    return field;
  });
}
