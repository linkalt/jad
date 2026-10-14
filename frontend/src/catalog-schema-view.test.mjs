import assert from 'node:assert/strict';
import {test} from 'node:test';

import {schemaFields} from './catalog-schema-view.ts';

test('each property becomes a field with its type, default and description', () => {
  const fields = schemaFields({
    required: ['model'],
    properties: {
      model: {type: 'string', description: 'The model.'},
      timeout: {type: ['integer', 'null'], default: 30},
      version: {const: 1},
      mode: {enum: ['a', 'b']},
      transport: {oneOf: []},
    },
  });
  assert.deepEqual(fields, [
    {name: 'model', isRequired: true, type: 'string', description: 'The model.'},
    {name: 'timeout', isRequired: false, type: 'integer or null', description: '', defaultValue: '30'},
    {name: 'version', isRequired: false, type: '1', description: ''},
    {name: 'mode', isRequired: false, type: '"a" | "b"', description: ''},
    {name: 'transport', isRequired: false, type: '', description: ''},
  ]);
});

test('a schema with no properties, or something that is not a schema, has no fields', () => {
  assert.deepEqual(schemaFields({}), []);
  assert.deepEqual(schemaFields(null), []);
  assert.deepEqual(schemaFields('x'), []);
  assert.deepEqual(schemaFields({properties: 3}), []);
});

test('a property that is not an object still lists, with no details', () => {
  assert.deepEqual(schemaFields({properties: {loose: true}}), [{name: 'loose', isRequired: false, type: '', description: ''}]);
});
