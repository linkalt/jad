import assert from 'node:assert/strict';
import {mkdirSync, readFileSync, rmSync, writeFileSync} from 'node:fs';
import {after, test} from 'node:test';

import {
  modelChoiceRows,
  modelOptionsForSession,
  resolveRunModelChoice,
  runModelBlock,
} from './model-selection.ts';

// The session list the backend wrote (backend-go internal/app TestAHiddenModelDefaultKeeps...):
// a profile whose own model_default names a model file that fails the model rules. The list
// carries model_fault, names the file, and has no model_default; the only offered model is the
// other one. REPAIRED_LIST is the same session after the user chose that model in the picker (the
// model-default route wrote it into the profile). Only the temporary folder of the run was
// replaced by /srv/jad.
const FAULT_LIST = "{\"sessions\":[{\"id\":\"demo\",\"session_id\":\"demo\",\"root_id\":\"local\",\"root_label\":\"local\",\"session_name\":\"demo\",\"category\":\"work\",\"turns\":1,\"profile_turns\":{\"dev/coder\":1,\"review/reviewer\":0},\"active_profile\":\"dev/coder\",\"default_profile\":\"dev/coder\",\"profiles\":[\"dev/coder\",\"review/reviewer\"],\"model_fault\":\"/srv/jad/models/openai/small.json: invalid JSON: unexpected end of JSON input\",\"profile_etag\":\"\\\"3d8050f44f9db790b318446533c8eeb9aaf91029b47862c6fb120105d4cafe6e\\\"\",\"models\":[\"openai/large\"],\"model_options\":[{\"id\":\"openai/large\",\"provider\":\"openai\"}],\"front_compaction\":{\"delete_mode\":\"permanent\",\"enabled\":true},\"usage\":{\"completed_responses\":1,\"responses_with_usage\":0,\"turn_count\":1,\"history_partial\":false,\"current\":{\"turn\":1,\"step\":1,\"response_id\":\"r1\"}},\"run\":{\"turn_id\":\"web-e96bddb5-5a25-46f2-ba73-c817be630047\",\"started_at\":\"2026-11-12T00:00:46.000Z\",\"status\":\"completed\"},\"runs_by_profile\":{\"dev/coder\":{\"turn_id\":\"web-e96bddb5-5a25-46f2-ba73-c817be630047\",\"started_at\":\"2026-11-12T00:00:46.000Z\",\"status\":\"completed\"}},\"model\":{\"provider\":\"unknown\",\"model\":\"demo\"}}]}";
const REPAIRED_LIST = "{\"sessions\":[{\"id\":\"demo\",\"session_id\":\"demo\",\"root_id\":\"local\",\"root_label\":\"local\",\"session_name\":\"demo\",\"category\":\"work\",\"turns\":5,\"profile_turns\":{\"dev/coder\":5,\"review/reviewer\":0},\"active_profile\":\"dev/coder\",\"default_profile\":\"dev/coder\",\"profiles\":[\"dev/coder\",\"review/reviewer\"],\"model_default\":\"openai/large\",\"profile_etag\":\"\\\"784afcc982cf4271814e00b63537a0d8fbc53c5acca8990d9cb98c18f08f22c9\\\"\",\"models\":[\"openai/large\"],\"model_options\":[{\"id\":\"openai/large\",\"provider\":\"openai\"}],\"front_compaction\":{\"delete_mode\":\"permanent\",\"enabled\":true},\"usage\":{\"completed_responses\":2,\"responses_with_usage\":0,\"turn_count\":5,\"history_partial\":false,\"current\":{\"turn\":5,\"step\":1,\"response_id\":\"r2\"}},\"run\":{\"turn_id\":\"web-7a138a3c-e660-42f3-883e-dae89545c4fb\",\"started_at\":\"2026-11-12T00:00:46.014Z\",\"status\":\"completed\"},\"runs_by_profile\":{\"dev/coder\":{\"turn_id\":\"web-7a138a3c-e660-42f3-883e-dae89545c4fb\",\"started_at\":\"2026-11-12T00:00:46.014Z\",\"status\":\"completed\"}},\"model\":{\"provider\":\"openai\",\"model\":\"model-large\"}}]}";

// session-mapping.ts is transpiled into a scratch folder with its imports pointed at the sources;
// only row-text (a style helper that needs the build's StyleX plugin) is replaced, by a stub that
// keeps the title it makes out of the way.
const scratch = new URL('../node_modules/.cache/model-fault-test/', import.meta.url);
const {transformWithOxc} = await import('vite');
mkdirSync(scratch, {recursive: true});
const mappingSource = readFileSync(new URL('./session-mapping.ts', import.meta.url), 'utf8')
  .replace("from './api'", `from '${new URL('./api.ts', import.meta.url).href}'`)
  .replace("from './row-text'", `from '${new URL('row-text-stub.mjs', scratch).href}'`)
  .replace("from './session-run-status'", `from '${new URL('./session-run-status.ts', import.meta.url).href}'`);
writeFileSync(new URL('row-text-stub.mjs', scratch), 'export const oneLine = text => String(text ?? "");\n');
writeFileSync(new URL('mapping.mjs', scratch), (await transformWithOxc(mappingSource, 'session-mapping.ts')).code);
const {sessionFromSnapshot} = await import(new URL('mapping.mjs', scratch).href);

const faultSession = sessionFromSnapshot(JSON.parse(FAULT_LIST).sessions[0], 'demo');
const repairedSession = sessionFromSnapshot(JSON.parse(REPAIRED_LIST).sessions[0], 'demo');

// The same record offering a model of another provider as well, which is where a silent
// preselection would redirect a run.
function withOtherProvider(session) {
  return {
    ...session,
    models: [...(session.models ?? []), 'anthropic/claude'],
    modelOptions: [...(session.modelOptions ?? []), {id: 'anthropic/claude', provider: 'anthropic'}],
  };
}

test('the mapped record carries the fault, names the file and has no default', () => {
  assert.match(faultSession.modelFault, /\/srv\/jad\/models\/openai\/small\.json: invalid JSON/);
  assert.equal(faultSession.modelDefault, undefined);
  assert.deepEqual(faultSession.models, ['openai/large']);
  assert.equal(repairedSession.modelFault, undefined);
  assert.equal(repairedSession.modelDefault, 'openai/large');
});

test('with the configured default hidden no model is chosen for the user, whichever models are offered', () => {
  assert.deepEqual(resolveRunModelChoice(faultSession), {});
  assert.deepEqual(resolveRunModelChoice(withOtherProvider(faultSession)), {});
  // The same record without the fault shows the first offered model, as it always did.
  const {modelFault, ...withoutFault} = withOtherProvider(faultSession);
  assert.ok(modelFault !== undefined);
  assert.equal(resolveRunModelChoice(withoutFault).option?.id, 'openai/large');
});

test('the normal default is unchanged: the configured model is the choice', () => {
  const choice = resolveRunModelChoice(withOtherProvider(repairedSession));
  assert.equal(choice.option?.id, 'openai/large');
  assert.equal(choice.option?.provider, 'openai');
});

test('a message that names no model is stopped, naming the file; the user\'s own model is not', () => {
  const block = runModelBlock(faultSession, undefined);
  assert.match(block, /\/srv\/jad\/models\/openai\/small\.json/);
  assert.match(block, /Choose a model/);
  assert.match(block, /was not sent/);
  assert.equal(runModelBlock(faultSession, 'openai/large'), undefined, 'an explicit model is the user\'s choice');
  assert.equal(runModelBlock(repairedSession, undefined), undefined);
  assert.equal(runModelBlock(undefined, undefined), undefined);
});

test('an explicit pick from the list is a row of the offered models with its own effort', () => {
  const options = modelOptionsForSession(withOtherProvider(faultSession));
  const rows = modelChoiceRows(options, undefined, undefined, '', true);
  assert.deepEqual(rows.map(row => [row.option.id, row.isSelected]), [
    ['openai/large', false],
    ['anthropic/claude', false],
  ]);
});

test('App stops a send before storing anything and keeps the draft', () => {
  const app = readFileSync(new URL('./App.tsx', import.meta.url), 'utf8');
  const submit = app.slice(app.indexOf('const submitMessage = useCallback('));
  const guard = submit.indexOf('runModelBlock(target.session, options.model)');
  assert.ok(guard > 0, 'submitMessage checks the model first');
  assert.ok(guard < submit.indexOf('setThreads('), 'the check comes before the message is stored');
  assert.ok(guard < submit.indexOf('streamSessionRun('), 'the check comes before the request');
  assert.match(submit.slice(guard, guard + 500), /return false;/, 'a stopped send keeps the draft');
  assert.match(app, /<ModelDrawer[\s\S]*?fault=\{modelFaultOf\(pane\.session\)\}/, 'the drawer is told the fault');
});

// The drawer is a React component: it is transpiled with the app's transformer into a scratch
// folder under node_modules, its relative imports pointed at the sources, and rendered to markup.
async function renderDrawer(props) {
  const React = await import('react');
  const {renderToStaticMarkup} = await import('react-dom/server');
  const source = readFileSync(new URL('./components/ModelDrawer.tsx', import.meta.url), 'utf8')
    .replace("from '../config.json'", `from '${new URL('./config.json', import.meta.url).href}' with {type: 'json'}`)
    .replace("from '../model-selection'", `from '${new URL('./model-selection.ts', import.meta.url).href}'`);
  const output = await transformWithOxc(source, 'ModelDrawer.tsx', {jsx: {runtime: 'automatic'}});
  mkdirSync(scratch, {recursive: true});
  writeFileSync(new URL('drawer.mjs', scratch), output.code);
  const {ModelDrawer} = await import(new URL('drawer.mjs', scratch).href);
  return renderToStaticMarkup(React.createElement(ModelDrawer, {sessionName: 's', onChange() {}, ...props}));
}

after(() => {
  rmSync(scratch, {recursive: true, force: true});
});

test('the drawer of a faulted session opens on the whole list, selects nothing and says why', async () => {
  const session = withOtherProvider(faultSession);
  const choice = resolveRunModelChoice(session);
  const html = await renderDrawer({
    options: modelOptionsForSession(session),
    selectedModelId: choice.option?.id,
    fault: session.modelFault,
  });
  assert.match(html, /data-model-fault/);
  assert.match(html, /small\.json: invalid JSON/);
  assert.match(html, /Search models/, 'the list is open');
  assert.equal((html.match(/data-model-row="choice"/g) ?? []).length, 2, 'both models are offered');
  assert.doesNotMatch(html, /data-model-row="selected"/, 'none is marked as the running one');
});

test('the drawer of a healthy session is unchanged: the running model alone, collapsed', async () => {
  const session = withOtherProvider(repairedSession);
  const html = await renderDrawer({
    options: modelOptionsForSession(session),
    selectedModelId: resolveRunModelChoice(session).option?.id,
  });
  assert.doesNotMatch(html, /data-model-fault/);
  assert.doesNotMatch(html, /Search models/);
  assert.equal((html.match(/data-model-row="selected"/g) ?? []).length, 1);
  assert.doesNotMatch(html, /anthropic\/claude/);
});
