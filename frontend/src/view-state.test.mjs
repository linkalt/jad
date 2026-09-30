import test from 'node:test';
import assert from 'node:assert/strict';

import {viewKey} from './session-views.ts';
import {canAttachRunToView, updateViewState} from './view-state.ts';

const session = 'root:session1';
const masterView = viewKey(session);
const profileView = viewKey(session, 'chatgpt');

test('master and named-profile state remain independent across updates and refreshes', () => {
  const masterMessages = [{id: 'master-1', text: 'stored default'}];
  const profileMessages = [{id: 'profile-1', text: 'named profile'}];
  let messagesByView = {};

  messagesByView = updateViewState(messagesByView, masterView, () => masterMessages);
  messagesByView = updateViewState(messagesByView, profileView, () => profileMessages);

  assert.deepEqual(messagesByView[masterView], masterMessages);
  assert.deepEqual(messagesByView[profileView], profileMessages);

  // A refresh of either view replaces only its own cached transcript.
  const refreshedProfileMessages = [...profileMessages, {id: 'profile-2', text: 'still profile'}];
  messagesByView = updateViewState(messagesByView, profileView, () => refreshedProfileMessages);
  assert.deepEqual(messagesByView[masterView], masterMessages);
  assert.deepEqual(messagesByView[profileView], refreshedProfileMessages);

  const refreshedMasterMessages = [...masterMessages, {id: 'master-2', text: 'still default'}];
  messagesByView = updateViewState(messagesByView, masterView, () => refreshedMasterMessages);
  assert.deepEqual(messagesByView[masterView], refreshedMasterMessages);
  assert.deepEqual(messagesByView[profileView], refreshedProfileMessages);
});

test('clearing one profile live state does not clear another profile', () => {
  const masterRun = {turnId: 'master-turn', pending: undefined};
  const profileRun = {turnId: 'profile-turn', pending: {id: 'profile-question'}};
  let runsByView = {};

  runsByView = updateViewState(runsByView, masterView, () => masterRun);
  runsByView = updateViewState(runsByView, profileView, () => profileRun);
  runsByView = updateViewState(runsByView, profileView, () => undefined);

  assert.equal(runsByView[masterView], masterRun);
  assert.equal(Object.hasOwn(runsByView, profileView), false);
});

test('an active run attachment for one view does not block the other profile', () => {
  const masterTurn = 'master-turn';
  const profileTurn = 'profile-turn';
  const masterAttachments = new Map([[masterView, masterTurn]]);
  const masterControllers = new Map([[masterView, {}]]);

  assert.equal(
    canAttachRunToView(masterAttachments, masterControllers, profileView, profileTurn),
    true,
  );
  assert.equal(
    canAttachRunToView(masterAttachments, masterControllers, masterView, masterTurn),
    false,
  );

  const profileAttachments = new Map([[profileView, profileTurn]]);
  const profileControllers = new Map([[profileView, {}]]);
  assert.equal(
    canAttachRunToView(profileAttachments, profileControllers, masterView, masterTurn),
    true,
  );
  assert.equal(
    canAttachRunToView(profileAttachments, profileControllers, profileView, profileTurn),
    false,
  );
});
