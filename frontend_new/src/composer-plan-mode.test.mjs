import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

const composer = readFileSync(new URL('./components/ComposerPane.tsx', import.meta.url), 'utf8');
const app = readFileSync(new URL('./App.tsx', import.meta.url), 'utf8');
const {default: config} = await import('./config.json', {with: {type: 'json'}});

test('plan mode is configured and on by default', () => {
  assert.equal(config.plan_mode_enabled, true);
  assert.equal(typeof config.plan_mode_placeholder, 'string');
  assert.ok(config.plan_mode_placeholder.length > 0);
});

test('the composer keeps plan mode per view and sends it with each message', () => {
  assert.match(composer, /planViews\[draftKey\] === true/, 'per-view state');
  assert.match(composer, /onSubmit\(text, sentImages, sentFiles, \{plan: isPlan\}\)/);
  // The /plan pick switches the composer's own mode instead of running an app action.
  assert.match(composer, /if \(selection\.name === 'plan'\) togglePlan\(\);\s*else onSlashCommand\?\.\(selection\.name\);/);
  assert.match(composer, /label=\{isPlan \? 'Turn off plan mode'/);
});

test('the app passes plan to the run request and lists /plan only when enabled', () => {
  const run = app.match(/void streamSessionRun\(sessionRef, buildRunInput\([\s\S]*?onRun:/);
  assert.ok(run, 'streamSessionRun call found');
  assert.match(run[0], /options\.plan === true \? \{plan: true\} : \{\}/);
  assert.match(app, /builtInSlashCommands\(appConfig\.plan_mode_enabled\)/);
  assert.match(app, /if \(name === 'plan'\) \{\s*\/\/[^\n]*\n\s*return;/);
});
