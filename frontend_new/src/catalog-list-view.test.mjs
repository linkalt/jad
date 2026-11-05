import assert from 'node:assert/strict';
import {test} from 'node:test';

import {FILTER_ALL, FILTER_NONE, catalogRows, categoryOf, filterCatalogFiles, filterOptions, filterValues, sessionsOf, sortCatalogRows} from './catalog-list-view.ts';

const models = [
  {id: 'openai/gpt-deep', etag: '1', name: 'Deep thinker'},
  {id: 'openai/small', etag: '2'},
  {id: 'local/llama', etag: '3', name: 'Llama'},
];
const sessions = [
  {id: 'alpha', etag: '1', name: 'Alpha', category: 'work'},
  {id: 'beta', etag: '2'},
  {id: 'gamma', etag: '3', category: 'work'},
];
const alphaRef = {id: 'alpha', name: 'Alpha', category: 'work'};
const gammaRef = {id: 'gamma', category: 'work'};
const homeRef = {id: 'home', name: 'Home', category: 'life'};
const looseRef = {id: 'loose'};
const profiles = [
  {id: 'dev/coder', etag: '1', name: 'Coder', sessions: [alphaRef, gammaRef]},
  {id: 'dev/review', etag: '2', sessions: [homeRef]},
  {id: 'solo/only', etag: '3', sessions: [looseRef]},
  {id: 'solo/spare', etag: '4'},
];
const ids = rows => rows.map(row => row.id);

test('a category is the folder part of the id, except for sessions, which set their own', () => {
  assert.equal(categoryOf('models', models[0]), 'openai');
  assert.equal(categoryOf('sessions', sessions[0]), 'work');
  assert.equal(categoryOf('sessions', sessions[1]), '');
  assert.equal(categoryOf('sessions', {id: 'x/y', etag: '1'}), '');
});

test('category options list each category once, sorted, with no-category last only when present', () => {
  assert.deepEqual(filterValues(filterOptions('models', models)), [FILTER_ALL, 'local', 'openai']);
  assert.deepEqual(filterValues(filterOptions('sessions', sessions)), [FILTER_ALL, 'work', FILTER_NONE]);
  assert.deepEqual(filterValues(filterOptions('models', [])), [FILTER_ALL]);
});

test('profiles filter by session, with one section per session category and sessions with none last', () => {
  const entries = filterOptions('profiles', profiles);
  assert.deepEqual(entries, [
    {value: FILTER_ALL, label: 'All sessions'},
    {type: 'section', title: 'life', options: [{value: 'home', label: 'Home'}]},
    {type: 'section', title: 'work', options: [{value: 'alpha', label: 'Alpha'}, {value: 'gamma', label: 'gamma'}]},
    {type: 'section', title: 'No category', options: [{value: 'loose', label: 'loose'}]},
    {value: FILTER_NONE, label: 'Not in any session'},
  ]);
});

test('profiles with no session offer no "not in any session" choice', () => {
  const used = profiles.slice(0, 3);
  assert.equal(filterValues(filterOptions('profiles', used)).includes(FILTER_NONE), false);
});

test('an empty query and no filter keep every file, sorted by category and then ID', () => {
  assert.deepEqual(ids(filterCatalogFiles('models', models, '  ', FILTER_ALL)), ['local/llama', 'openai/gpt-deep', 'openai/small']);
  assert.deepEqual(ids(filterCatalogFiles('sessions', sessions, '', FILTER_ALL)), ['beta', 'alpha', 'gamma']);
});

test('search matches the name, the id and the category, and every term must match', () => {
  assert.deepEqual(ids(filterCatalogFiles('models', models, 'DEEP', FILTER_ALL)), ['openai/gpt-deep']);
  assert.deepEqual(ids(filterCatalogFiles('models', models, 'small', FILTER_ALL)), ['openai/small']);
  assert.deepEqual(ids(filterCatalogFiles('models', models, 'local', FILTER_ALL)), ['local/llama']);
  assert.deepEqual(ids(filterCatalogFiles('models', models, 'llama local', FILTER_ALL)), ['local/llama']);
  assert.deepEqual(ids(filterCatalogFiles('models', models, 'llama openai', FILTER_ALL)), []);
});

test('the category choice narrows the list, and no-category keeps the files without one', () => {
  assert.deepEqual(ids(filterCatalogFiles('models', models, '', 'local')), ['local/llama']);
  assert.deepEqual(ids(filterCatalogFiles('sessions', sessions, '', 'work')), ['alpha', 'gamma']);
  assert.deepEqual(ids(filterCatalogFiles('sessions', sessions, '', FILTER_NONE)), ['beta']);
  assert.deepEqual(ids(filterCatalogFiles('sessions', sessions, 'beta', 'work')), []);
});

test('a session choice keeps the profiles that session can use, and "not in any session" keeps the rest', () => {
  assert.deepEqual(ids(filterCatalogFiles('profiles', profiles, '', 'alpha')), ['dev/coder']);
  assert.deepEqual(ids(filterCatalogFiles('profiles', profiles, '', 'home')), ['dev/review']);
  assert.deepEqual(ids(filterCatalogFiles('profiles', profiles, '', FILTER_NONE)), ['solo/spare']);
});

test('profile search also matches the names of its sessions', () => {
  assert.deepEqual(ids(filterCatalogFiles('profiles', profiles, 'home', FILTER_ALL)), ['dev/review']);
  assert.deepEqual(ids(filterCatalogFiles('profiles', profiles, 'alpha gamma', FILTER_ALL)), ['dev/coder']);
});

test('the Session column joins the session names, falling back to the ID', () => {
  assert.equal(sessionsOf(profiles[0]), 'Alpha, gamma');
  assert.equal(sessionsOf(profiles[3]), '');
});

test('filtering does not reorder or change the list it was given', () => {
  const before = ids(models);
  filterCatalogFiles('models', models, '', FILTER_ALL);
  assert.deepEqual(ids(models), before);
});

test('rows carry one value per column, so a column sorts by its own key', () => {
  const rows = catalogRows('models', models);
  assert.deepEqual(rows.map(row => [row.category, row.id, row.name]), [
    ['openai', 'openai/gpt-deep', 'Deep thinker'],
    ['openai', 'openai/small', ''],
    ['local', 'local/llama', 'Llama'],
  ]);
  assert.equal(catalogRows('profiles', profiles)[0].sessions, 'Alpha, gamma');
});

test('each row carries its version, so a delete from the list can send it', () => {
  assert.deepEqual(catalogRows('models', models).map(row => [row.id, row.etag]), [
    ['openai/gpt-deep', '1'],
    ['openai/small', '2'],
    ['local/llama', '3'],
  ]);
  assert.equal(catalogRows('sessions', sessions)[0].etag, '1');
});

test('sorting follows the entries in turn and honours the direction', () => {
  const rows = catalogRows('models', models);
  const order = sort => sortCatalogRows(rows, sort).map(row => row.id);
  assert.deepEqual(order([{sortKey: 'category', direction: 'ascending'}]), ['local/llama', 'openai/gpt-deep', 'openai/small']);
  assert.deepEqual(order([{sortKey: 'category', direction: 'descending'}]), ['openai/gpt-deep', 'openai/small', 'local/llama']);
  assert.deepEqual(order([{sortKey: 'category', direction: 'ascending'}, {sortKey: 'id', direction: 'descending'}]), ['local/llama', 'openai/small', 'openai/gpt-deep']);
  assert.deepEqual(order([{sortKey: 'name', direction: 'descending'}]), ['local/llama', 'openai/gpt-deep', 'openai/small']);
});

test('sorting reads digits as numbers, ignores case, skips unknown keys and leaves its input alone', () => {
  const rows = catalogRows('models', [
    {id: 'a/m10', etag: '1'},
    {id: 'a/M2', etag: '2'},
    {id: 'a/m1', etag: '3'},
  ]);
  const before = rows.map(row => row.id);
  assert.deepEqual(sortCatalogRows(rows, [{sortKey: 'id', direction: 'ascending'}]).map(row => row.id), ['a/m1', 'a/M2', 'a/m10']);
  assert.deepEqual(sortCatalogRows(rows, [{sortKey: 'nope', direction: 'descending'}]).map(row => row.id), ['a/m1', 'a/M2', 'a/m10']);
  assert.deepEqual(rows.map(row => row.id), before);
});

test('catalogRows lists a model\'s efforts and sorts by them', () => {
  const rows = catalogRows('models', [{id: 'openai/b', efforts: ['low', 'high']}, {id: 'openai/a'}]);
  assert.deepEqual(rows.map(row => row.efforts), ['low, high', '']);
  assert.deepEqual(sortCatalogRows(rows, [{sortKey: 'efforts', direction: 'descending'}]).map(row => row.id), ['openai/b', 'openai/a']);
});
