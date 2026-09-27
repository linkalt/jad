import assert from 'node:assert/strict';
import test from 'node:test';

import {filterLibraryItems} from './library-search.ts';

const items = [
  {
    id: 'creative',
    name: 'Creative and Content Workflow',
    description: 'Use for original writing, messaging, campaign concepts and creative direction.',
    source: 'agent-workflows/08-creative-content.md',
  },
  {
    id: 'engineering',
    name: 'Software Engineering Workflow',
    description: 'Use for repository changes, debugging and code review.',
    source: 'agent-workflows/04-software-engineering.md',
  },
  {
    id: 'general',
    name: 'General-Purpose Agent Operating System',
    description: '',
    source: 'agent-operating-systems/general-purpose.md',
  },
];

const ids = (query) => filterLibraryItems(items, query).map(item => item.id);

test('an empty or whitespace-only query keeps every item, in order', () => {
  assert.deepEqual(ids(''), ['creative', 'engineering', 'general']);
  assert.deepEqual(ids('   '), ['creative', 'engineering', 'general']);
});

test('matching ignores case and reaches name, description and source path', () => {
  assert.deepEqual(ids('CREATIVE'), ['creative']);
  assert.deepEqual(ids('debugging'), ['engineering']);
  assert.deepEqual(ids('general-purpose.md'), ['general']);
});

test('every term has to match somewhere, in any order', () => {
  assert.deepEqual(ids('workflow engineering'), ['engineering']);
  assert.deepEqual(ids('engineering workflow'), ['engineering']);
  assert.deepEqual(ids('workflow unrelated'), []);
});

test('a query that matches nothing returns nothing', () => {
  assert.deepEqual(ids('zzz'), []);
});

test('filtering never touches the input list', () => {
  const before = items.map(item => item.id);
  filterLibraryItems(items, 'creative engineering');
  assert.deepEqual(items.map(item => item.id), before);
});
