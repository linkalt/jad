import assert from 'node:assert/strict';
import {existsSync, readFileSync, readdirSync} from 'node:fs';
import test from 'node:test';

import appConfig from './config.json' with {type: 'json'};
import {categorySectionLabel, groupByCategory, sessionListGroups} from './session-views.ts';

const src = new URL('./', import.meta.url);
const read = name => readFileSync(new URL(name, src), 'utf8');
const app = read('App.tsx');
const navigation = read('components/SessionNavigation.tsx');

const session = (id, category) => ({
  id,
  title: id,
  time: '',
  turns: 0,
  state: 'success',
  sessionRef: id,
  profiles: ['a/p'],
  activeProfile: undefined,
  ...(category === undefined ? {} : {category}),
});

test('sessions without a category are listed under Others', () => {
  assert.equal(appConfig.session_uncategorized_title, 'Others');
  const sections = groupByCategory(
    sessionListGroups([session('work-1', 'work'), session('loose-1'), session('work-2', 'work'), session('loose-2')]),
  );
  assert.deepEqual(
    sections.map(section => [categorySectionLabel(section, appConfig.session_uncategorized_title), section.groups.map(group => group.sessionRef)]),
    [['work', ['work-1', 'work-2']], ['Others', ['loose-1', 'loose-2']]],
  );
});

test('a named category keeps its own heading; only the unnamed section takes the fallback', () => {
  const [named, unnamed] = groupByCategory(sessionListGroups([session('a', 'lab'), session('b')]));
  assert.equal(categorySectionLabel(named, appConfig.session_uncategorized_title), 'lab');
  assert.equal(categorySectionLabel(unnamed, appConfig.session_uncategorized_title), 'Others');
});

test('the rail takes that heading from the configuration, not from a literal', () => {
  assert.match(navigation, /categorySectionLabel\(section, appConfig\.session_uncategorized_title\)/);
  assert.equal(navigation.includes('Recent runs'), false);
  assert.deepEqual(
    readdirSync(src).filter(name => name.endsWith('.ts') || name.endsWith('.tsx') || name.endsWith('.json')).filter(name => read(name).includes('Recent runs')),
    [],
  );
});

test('the mock sessions are gone: no sample data module, no sample rows anywhere', () => {
  assert.equal(existsSync(new URL('sample-data.ts', src)), false, 'sample-data.ts was deleted');
  for (const name of ['App.tsx', 'App.test.mjs']) {
    if (!existsSync(new URL(name, src))) continue;
    const text = read(name);
    for (const gone of ['sample-data', 'initialSessions', 'initialThreads', 'baseReasoning', 'defaultTools', 'getTraceForSteps']) {
      assert.equal(text.includes(gone), false, `${name} no longer refers to ${gone}`);
    }
  }
  assert.equal(
    readdirSync(src)
      .filter(name => /\.(ts|tsx)$/.test(name))
      .filter(name => read(name).includes("'sample'")).length,
    0,
    'no source file names a sample session source any more',
  );
});

test('the mock run went with them: no fake reply, no delay setting, no transport toggle', () => {
  assert.equal(app.includes('mock_run_delay_ms'), false);
  assert.equal('mock_run_delay_ms' in appConfig, false, 'the config key is gone, not merely unused');
  const settings = read('components/RunSettingsPanel.tsx');
  assert.equal(settings.includes('Mock run delay'), false);
  assert.equal(settings.includes("'mock'"), false);
  // Sending without a server session is refused rather than simulated.
  assert.match(app, /target\.session\?\.sessionRef === undefined/);
  assert.equal(app.includes('The mock run is complete'), false);
});

test('a session row no longer claims to be live or a sample', () => {
  const types = read('app-types.ts');
  assert.equal(/source: 'live' \| 'sample'/.test(types), false);
  assert.equal(app.includes("session.source === 'live'"), false);
  assert.equal(read('session-mapping.ts').includes('source:'), false);
});
