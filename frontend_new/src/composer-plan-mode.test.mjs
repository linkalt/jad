import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

const composer = readFileSync(new URL('./components/ComposerPane.tsx', import.meta.url), 'utf8');
const app = readFileSync(new URL('./App.tsx', import.meta.url), 'utf8');
const {default: config} = await import('./config.json', {with: {type: 'json'}});

test('plan mode is configured and on by default', () => {
  assert.equal(config.plan_mode_enabled, true);
  for (const key of ['plan_mode_execute_label', 'plan_mode_execute_message']) {
    assert.equal(typeof config[key], 'string');
    assert.ok(config[key].length > 0, key);
  }
  assert.equal(typeof config.plan_mode_placeholder, 'string');
  assert.ok(config.plan_mode_placeholder.length > 0);
});

test('the composer keeps plan mode per view and sends it with each message', () => {
  assert.match(composer, /planViews\[draftKey\] === true/, 'per-view state');
  assert.match(composer, /const options: ComposerSubmitOptions = \{\s*plan: isPlan,/);
  assert.match(composer, /onSubmit\(text, sentImages, sentFiles, options\)/);
  // The /plan pick switches the composer's own mode instead of running an app action.
  assert.match(composer, /if \(selection\.name === 'plan'\) togglePlan\(\);\s*else onSlashCommand\?\.\(selection\.name\);/);
  assert.match(composer, /label=\{isPlan \? 'Turn off plan mode'/);
});

test('the app passes plan to the run request and lists /plan only when enabled', () => {
  const run = app.match(/void streamSessionRun\(sessionRef, [\s\S]*?onRun:/);
  assert.ok(run, 'streamSessionRun call found');
  assert.match(run[0], /options\.plan === true \? \{plan: true\} : \{\}/);
  assert.match(app, /builtInSlashCommands\(appConfig\.plan_mode_enabled\)/);
  assert.match(app, /if \(name === 'plan'\) \{\s*\/\/[^\n]*\n\s*return;/);
});

test('Execute plan leaves plan mode and sends execute_plan with the draft text only', () => {
  const execute = composer.match(/const executePlan = useCallback\([\s\S]*?\n  \}, \[/);
  assert.ok(execute, 'executePlan found');
  assert.match(execute[0], /const options: ComposerSubmitOptions = \{\s*plan: false,\s*executePlan: true,/);
  assert.match(execute[0], /onSubmit\(text, \[\], \[\], options\)/);
  // A pending custom command's model and tools are not dropped (they used to be cleared unsent).
  assert.match(execute[0], /pendingCommand\?\.model === undefined \? \{\} : \{model: pendingCommand\.model\}/);
  assert.match(execute[0], /pendingCommand\?\.tools === undefined \? \{\} : \{tools: pendingCommand\.tools\}/);
  assert.match(execute[0], /\[draftKey\]: false/);
  // The button shows while plan mode is on and nothing runs.
  assert.match(composer, /\{isPlan && !isRunning \? \(\s*<Button\s*label=\{appConfig\.plan_mode_execute_label\}/);
  const run = app.match(/void streamSessionRun\(sessionRef, [\s\S]*?onRun:/);
  assert.match(run[0], /executePlan \? trimmedValue : buildRunInput/);
  assert.match(run[0], /\.\.\.\(executePlan \? \{executePlan: true\} : \{\}\)/);
  assert.match(app, /!trimmedValue && images\.length === 0 && files\.length === 0 && !executePlan/);
});

test('a custom command\'s options ride on the next message until sent or removed', () => {
  assert.match(composer, /applyCommandOptions\(command\.command, expanded\.options\)/);
  assert.match(composer, /if \(options\.plan === true && isPlanModeEnabled\)/);
  assert.match(composer, /<Token\s*size="sm"\s*label=\{commandOptionsLabel\(pendingCommand\)\}\s*onRemove=\{clearPendingCommand\}/);
  const run = app.match(/void streamSessionRun\(sessionRef, [\s\S]*?onRun:/);
  assert.match(run[0], /options\.model === undefined \? \{\} : \{model: options\.model\}/);
  assert.match(run[0], /options\.tools === undefined \? \{\} : \{tools: options\.tools\}/);
});
