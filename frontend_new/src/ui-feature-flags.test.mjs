import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

const read = path => readFileSync(new URL(path, import.meta.url), 'utf8');
const app = read('./App.tsx');
const viewPane = read('./components/ViewPane.tsx');
const catalogEditor = read('./components/CatalogEditorDialog.tsx');
const {default: config} = await import('./config.json', {with: {type: 'json'}});

test('each newer UI control has a flag in config.json', () => {
  assert.equal(config.fork_messages_enabled, true);
  assert.equal(config.duplicate_enabled, true);
  // The watched-resources panel is off by default, like the MCP change note it feeds.
  assert.equal(config.resource_watch_enabled, false);
  // These already had flags; they stay.
  for (const key of ['plan_mode_enabled', 'slash_menu_mcp_prompts_enabled', 'slash_menu_mcp_resources_enabled']) {
    assert.equal(typeof config[key], 'boolean', key);
  }
});

test('fork_messages_enabled false leaves the transcript without fork buttons', () => {
  assert.match(app, /onForkMessage=\{\(\(\) => \{\s*if \(!appConfig\.fork_messages_enabled \|\|/);
});

test('duplicate_enabled false hides Duplicate in the row menu and the catalog editor', () => {
  assert.match(app, /onDuplicate=\{appConfig\.duplicate_enabled \? duplicateRow : undefined\}/);
  assert.match(catalogEditor, /\{appConfig\.duplicate_enabled \? \(\s*<Button label="Duplicate"/);
});

test('resource_watch_enabled false hides the watched-resources toggle and panel', () => {
  assert.match(viewPane, /onToggleResourcePanel=\{appConfig\.resource_watch_enabled \? \(\) => togglePanel\('resources'\) : undefined\}/);
  assert.match(viewPane, /\{appConfig\.resource_watch_enabled && panel === 'resources' && \(/);
});
