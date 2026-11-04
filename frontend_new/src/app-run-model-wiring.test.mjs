import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

// The profile's model_default is the single source of truth for the drawer and
// the run: a drawer pick is saved to the profile and the sessions are refreshed;
// the run request carries no model.
const app = readFileSync(new URL('./App.tsx', import.meta.url), 'utf8');

test('App keeps no per-profile model pick map', () => {
  assert.ok(!/runModelChoices|setRunModelChoices/.test(app), 'no local pick state');
  assert.ok(!/resolveRunModelSelection/.test(app), 'the stored-pick resolver is gone');
});

test('the drawer pick is saved to the profile and the sessions are refreshed', () => {
  const save = app.match(/const saveModelDefault = async[\s\S]*?\n {2}\};/);
  assert.ok(save, 'saveModelDefault exists');
  assert.match(save[0], /updateSessionProfileModelDefault\(/);
  assert.match(save[0], /formatRunModel\(/);
  assert.match(save[0], /refreshSessionsRef\.current\?\.\(\)/);
  assert.match(save[0], /finally \{\s*setIsModelDefaultUpdating\(false\);/);
  // The drawer writes through the pane it was opened in, so its pick lands on
  // that pane's profile rather than on whichever pane happens to hold focus.
  assert.match(save[0], /target\.sessionRef/, 'the write uses its own target');
  assert.match(save[0], /target\.profile/, 'the write uses its own profile');
  assert.ok(!/activeSession|activeProfile|activeRowSession/.test(save[0]), 'no active-view read');
  assert.match(
    app,
    /<ModelDrawer[\s\S]*?onChange=\{choice =>\s*void saveModelDefault\(choice, \{/,
  );
});

test('the run request passes no model of the drawer: only a custom command\'s, for that message', () => {
  const run = app.match(/void streamSessionRun\(sessionRef, [\s\S]*?onRun:/);
  assert.ok(run, 'streamSessionRun call found');
  const models = run[0].match(/\bmodel\b[^,\n]*/g) ?? [];
  assert.deepEqual(models, ['model === undefined ? {} : {model: options.model})'], 'the only model is the submit option');
  assert.ok(!/\brunModel\b/.test(app), 'no runModel variable');
});
